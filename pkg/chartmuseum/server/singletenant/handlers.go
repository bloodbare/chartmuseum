package singletenant

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/kubernetes-helm/chartmuseum/pkg/repo"

	"github.com/gin-contrib/location"
	"github.com/gin-gonic/gin"
)

var (
	objectSavedResponse        = gin.H{"saved": true}
	objectDeletedResponse      = gin.H{"deleted": true}
	notFoundErrorResponse      = gin.H{"error": "not found"}
	badExtensionErrorResponse  = gin.H{"error": "unsupported file extension"}
	alreadyExistsErrorResponse = gin.H{"error": "file already exists"}
	healthCheckResponse        = gin.H{"healthy": true}
	welcomePageHTMLTemplate    = `<!DOCTYPE html>
<html>
<head>
<title>Welcome to ChartMuseum!</title>
<style>
    body {
        width: 35em;
        margin: 0 auto;
        font-family: Tahoma, Verdana, Arial, sans-serif;
    }
</style>
</head>
<body>
<h1>Welcome to ChartMuseum!</h1>
<p>If you see this page, the ChartMuseum web server is successfully installed and
working.</p>

<p>To add this as a local chart repository, please run the following command:</p>
<pre>helm repo add chartmuseum %s</pre>

<p>For online documentation and support please refer to the
<a href="https://github.com/kubernetes-helm/chartmuseum">GitHub project</a>.<br/>

<p><em>Thank you for using ChartMuseum.</em></p>
</body>
</html>
	`
)

type (
	packageOrProvenanceFile struct {
		filename string
		content  []byte
		field    string // file was extracted from this form field
	}
	filenameFromContentFn func([]byte) (string, error)
)

func (server *SingleTenantServer) getWelcomePageHandler(c *gin.Context) {
	url := location.Get(c)
	c.Data(200, "text/html", []byte(fmt.Sprintf(welcomePageHTMLTemplate, url)))
}

func (server *SingleTenantServer) getHealthCheckHandler(c *gin.Context) {
	c.JSON(200, healthCheckResponse)
}

func (server *SingleTenantServer) getIndexFileRequestHandler(c *gin.Context) {
	log := server.Logger.ContextLoggingFn(c)
	index, err := server.syncRepositoryIndex(log)
	if err != nil {
		c.JSON(500, errorResponse(err))
		return
	}
	c.Data(200, repo.IndexFileContentType, index.Raw)
}

func (server *SingleTenantServer) getAllChartsRequestHandler(c *gin.Context) {
	log := server.Logger.ContextLoggingFn(c)
	index, err := server.syncRepositoryIndex(log)
	if err != nil {
		c.JSON(500, errorResponse(err))
		return
	}
	c.JSON(200, index.Entries)
}

func (server *SingleTenantServer) getChartRequestHandler(c *gin.Context) {
	name := c.Param("name")
	log := server.Logger.ContextLoggingFn(c)
	index, err := server.syncRepositoryIndex(log)
	if err != nil {
		c.JSON(500, errorResponse(err))
		return
	}
	chart := index.Entries[name]
	if chart == nil {
		c.JSON(404, notFoundErrorResponse)
		return
	}
	c.JSON(200, chart)
}

func (server *SingleTenantServer) getChartVersionRequestHandler(c *gin.Context) {
	name := c.Param("name")
	version := c.Param("version")
	if version == "latest" {
		version = ""
	}
	log := server.Logger.ContextLoggingFn(c)
	index, err := server.syncRepositoryIndex(log)
	if err != nil {
		c.JSON(500, errorResponse(err))
		return
	}
	chartVersion, err := index.Get(name, version)
	if err != nil {
		c.JSON(404, notFoundErrorResponse)
		return
	}
	c.JSON(200, chartVersion)
}

func (server *SingleTenantServer) deleteChartVersionRequestHandler(c *gin.Context) {
	name := c.Param("name")
	version := c.Param("version")
	filename := repo.ChartPackageFilenameFromNameVersion(name, version)
	server.Logger.Debugc(c,"Deleting package from storage",
		"package", filename,
	)
	err := server.StorageBackend.DeleteObject(filename)
	if err != nil {
		c.JSON(404, notFoundErrorResponse)
		return
	}
	provFilename := repo.ProvenanceFilenameFromNameVersion(name, version)
	server.StorageBackend.DeleteObject(provFilename) // ignore error here, may be no prov file
	c.JSON(200, objectDeletedResponse)
}

func (server *SingleTenantServer) getStorageObjectRequestHandler(c *gin.Context) {
	filename := c.Param("filename")
	isChartPackage := strings.HasSuffix(filename, repo.ChartPackageFileExtension)
	isProvenanceFile := strings.HasSuffix(filename, repo.ProvenanceFileExtension)
	if !isChartPackage && !isProvenanceFile {
		c.JSON(500, badExtensionErrorResponse)
		return
	}
	object, err := server.StorageBackend.GetObject(filename)
	if err != nil {
		c.JSON(404, notFoundErrorResponse)
		return
	}
	if isProvenanceFile {
		c.Data(200, repo.ProvenanceFileContentType, object.Content)
		return
	}
	c.Data(200, repo.ChartPackageContentType, object.Content)
}

func (server *SingleTenantServer) extractAndValidateFormFile(req *http.Request, field string, fnFromContent filenameFromContentFn) (*packageOrProvenanceFile, int, error) {
	file, header, _ := req.FormFile(field)
	var ppf *packageOrProvenanceFile
	if file == nil || header == nil {
		return ppf, 200, nil // field is not present
	}
	buf := bytes.NewBuffer(nil)
	_, err := io.Copy(buf, file)
	if err != nil {
		return ppf, 500, err // IO error
	}
	content := buf.Bytes()
	filename, err := fnFromContent(content)
	if err != nil {
		return ppf, 400, err // validation error (bad request)
	}
	if !server.AllowOverwrite {
		_, err = server.StorageBackend.GetObject(filename)
		if err == nil {
			return ppf, 409, fmt.Errorf("%s already exists", filename) // conflict
		}
	}
	return &packageOrProvenanceFile{filename, content, field}, 200, nil
}

func (server *SingleTenantServer) postPackageAndProvenanceRequestHandler(c *gin.Context) {
	var ppFiles []*packageOrProvenanceFile

	type fieldFuncPair struct {
		field string
		fn    filenameFromContentFn
	}

	ffp := []fieldFuncPair{
		{server.ChartPostFormFieldName, repo.ChartPackageFilenameFromContent},
		{server.ProvPostFormFieldName, repo.ProvenanceFilenameFromContent},
	}

	for _, ff := range ffp {
		ppf, status, err := server.extractAndValidateFormFile(c.Request, ff.field, ff.fn)
		if err != nil {
			c.JSON(status, errorResponse(err))
			return
		}
		if ppf != nil {
			ppFiles = append(ppFiles, ppf)
		}
	}

	if len(ppFiles) == 0 {
		c.JSON(400, errorResponse(
			fmt.Errorf("no package or provenance file found in form fields %s and %s",
				server.ChartPostFormFieldName, server.ProvPostFormFieldName)))
		return
	}

	// At this point input is presumed valid, we now proceed to store it
	var storedFiles []*packageOrProvenanceFile
	for _, ppf := range ppFiles {
		server.Logger.Debugc(c,"Adding file to storage (form field)",
			"filename", ppf.filename,
			"field", ppf.field,
		)
		err := server.StorageBackend.PutObject(ppf.filename, ppf.content)
		if err == nil {
			storedFiles = append(storedFiles, ppf)
		} else {
			// Clean up what's already been saved
			for _, ppf := range storedFiles {
				server.StorageBackend.DeleteObject(ppf.filename)
			}
			c.JSON(500, errorResponse(err))
		}
	}
	c.JSON(201, objectSavedResponse)
}

func (server *SingleTenantServer) postRequestHandler(c *gin.Context) {
	if c.ContentType() == "multipart/form-data" {
		server.postPackageAndProvenanceRequestHandler(c) // new route handling form-based chart and/or prov files
	} else {
		server.postPackageRequestHandler(c) // classic binary data, chart package only route
	}
}

func (server *SingleTenantServer) postPackageRequestHandler(c *gin.Context) {
	content, err := c.GetRawData()
	if err != nil {
		c.JSON(500, errorResponse(err))
		return
	}
	filename, err := repo.ChartPackageFilenameFromContent(content)
	if err != nil {
		c.JSON(500, errorResponse(err))
		return
	}
	if !server.AllowOverwrite {
		_, err = server.StorageBackend.GetObject(filename)
		if err == nil {
			c.JSON(409, alreadyExistsErrorResponse)
			return
		}
	}
	server.Logger.Debugc(c,"Adding package to storage",
		"package", filename,
	)
	err = server.StorageBackend.PutObject(filename, content)
	if err != nil {
		c.JSON(500, errorResponse(err))
		return
	}
	c.JSON(201, objectSavedResponse)
}

func (server *SingleTenantServer) postProvenanceFileRequestHandler(c *gin.Context) {
	content, err := c.GetRawData()
	if err != nil {
		c.JSON(500, errorResponse(err))
		return
	}
	filename, err := repo.ProvenanceFilenameFromContent(content)
	if err != nil {
		c.JSON(500, errorResponse(err))
		return
	}
	if !server.AllowOverwrite {
		_, err = server.StorageBackend.GetObject(filename)
		if err == nil {
			c.JSON(409, alreadyExistsErrorResponse)
			return
		}
	}
	server.Logger.Debugc(c,"Adding provenance file to storage",
		"provenance_file", filename,
	)
	err = server.StorageBackend.PutObject(filename, content)
	if err != nil {
		c.JSON(500, errorResponse(err))
		return
	}
	c.JSON(201, objectSavedResponse)
}

func errorResponse(err error) map[string]interface{} {
	errResp := gin.H{"error": fmt.Sprintf("%s", err)}
	return errResp
}
