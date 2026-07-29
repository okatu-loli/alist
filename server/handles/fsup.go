package handles

import (
	"github.com/alist-org/alist/v3/internal/ctxkey"
	"io"
	"net/url"
	stdpath "path"
	"strconv"
	"time"

	"github.com/alist-org/alist/v3/internal/fs"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/internal/stream"
	"github.com/alist-org/alist/v3/internal/task"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

func getLastModified(c *gin.Context) time.Time {
	now := time.Now()
	lastModifiedStr := c.GetHeader("Last-Modified")
	lastModifiedMillisecond, err := strconv.ParseInt(lastModifiedStr, 10, 64)
	if err != nil {
		return now
	}
	lastModified := time.UnixMilli(lastModifiedMillisecond)
	return lastModified
}

// FsStream uploads a file by streaming the raw request body directly (no multipart encoding).
// @Summary Upload file (raw stream)
// @Description Runs behind middlewares.FsUp and the upload rate limiter. The destination path is joined with the caller's base path. If As-Task is true, the upload is queued as a background task instead of completing synchronously.
// @Tags fs
// @Security ApiKeyAuth
// @Accept application/octet-stream
// @Produce json
// @Param File-Path header string true "URL-escaped destination path, joined with the caller's base path"
// @Param Content-Length header int true "size in bytes of the uploaded content"
// @Param Last-Modified header int false "modification time in Unix milliseconds; defaults to now if absent/invalid"
// @Param As-Task header bool false "if \"true\", queue the upload as a background task"
// @Param Overwrite header bool false "if \"false\", reject the upload when the destination already exists (default: allow overwrite)"
// @Param Content-Type header string false "MIME type of the uploaded content; guessed from the file name if absent"
// @Param X-File-Md5 header string false "expected MD5 hash of the content"
// @Param X-File-Sha1 header string false "expected SHA1 hash of the content"
// @Param X-File-Sha256 header string false "expected SHA256 hash of the content"
// @Success 200 {object} common.Resp[gin.H] "data: {} or {task: TaskInfo} when As-Task is true"
// @Failure 400 {object} common.Resp[any] "bad File-Path or Content-Length"
// @Failure 403 {object} common.Resp[any] "no permission or file exists"
// @Failure 500 {object} common.Resp[any]
// @Router /fs/put [put]
// @Router /v4/fs/put [put]
func FsStream(c *gin.Context) {
	path := c.GetHeader("File-Path")
	path, err := url.PathUnescape(path)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	asTask := c.GetHeader("As-Task") == "true"
	overwrite := c.GetHeader("Overwrite") != "false"
	user := c.MustGet(ctxkey.User).(*model.User)
	path, err = user.JoinPath(path)
	if err != nil {
		common.ErrorResp(c, err, 403)
		return
	}
	if !overwrite {
		if res, _ := fs.Get(c, path, &fs.GetArgs{NoLog: true}); res != nil {
			_, _ = utils.CopyWithBuffer(io.Discard, c.Request.Body)
			common.ErrorI18n(c, "fsmanage.file_exists", 403, "name", stdpath.Base(path))
			return
		}
	}
	dir, name := stdpath.Split(path)
	sizeStr := c.GetHeader("Content-Length")
	size, err := strconv.ParseInt(sizeStr, 10, 64)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	h := make(map[*utils.HashType]string)
	if md5 := c.GetHeader("X-File-Md5"); md5 != "" {
		h[utils.MD5] = md5
	}
	if sha1 := c.GetHeader("X-File-Sha1"); sha1 != "" {
		h[utils.SHA1] = sha1
	}
	if sha256 := c.GetHeader("X-File-Sha256"); sha256 != "" {
		h[utils.SHA256] = sha256
	}
	mimetype := c.GetHeader("Content-Type")
	if len(mimetype) == 0 {
		mimetype = utils.GetMimeType(name)
	}
	s := &stream.FileStream{
		Obj: &model.Object{
			Name:     name,
			Size:     size,
			Modified: getLastModified(c),
			HashInfo: utils.NewHashInfoByMap(h),
		},
		Reader:       c.Request.Body,
		Mimetype:     mimetype,
		WebPutAsTask: asTask,
	}
	var t task.TaskExtensionInfo
	if asTask {
		t, err = fs.PutAsTask(c, dir, s)
	} else {
		err = fs.PutDirectly(c, dir, s, true)
	}
	defer c.Request.Body.Close()
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	if t == nil {
		if n, _ := io.ReadFull(c.Request.Body, []byte{0}); n == 1 {
			_, _ = utils.CopyWithBuffer(io.Discard, c.Request.Body)
		}
		common.SuccessResp(c)
		return
	}
	common.SuccessResp(c, gin.H{
		"task": getTaskInfo(c, t),
	})
}

// FsForm uploads a file via multipart/form-data.
// @Summary Upload file (multipart form)
// @Description Runs behind middlewares.FsUp and the upload rate limiter. The destination path is joined with the caller's base path. Rejected if the target storage has NoUpload set. If As-Task is true, the upload is queued as a background task instead of completing synchronously.
// @Tags fs
// @Security ApiKeyAuth
// @Accept multipart/form-data
// @Produce json
// @Param File-Path header string true "URL-escaped destination path, joined with the caller's base path"
// @Param Last-Modified header int false "modification time in Unix milliseconds; defaults to now if absent/invalid"
// @Param As-Task header bool false "if \"true\", queue the upload as a background task"
// @Param Overwrite header bool false "if \"false\", reject the upload when the destination already exists (default: allow overwrite)"
// @Param X-File-Md5 header string false "expected MD5 hash of the content"
// @Param X-File-Sha1 header string false "expected SHA1 hash of the content"
// @Param X-File-Sha256 header string false "expected SHA256 hash of the content"
// @Param file formData file true "file content"
// @Success 200 {object} common.Resp[gin.H] "data: {} or {task: TaskInfo} when As-Task is true"
// @Failure 400 {object} common.Resp[any] "bad File-Path or storage lookup failure"
// @Failure 403 {object} common.Resp[any] "no permission or file exists"
// @Failure 405 {object} common.Resp[any] "storage does not support upload"
// @Failure 500 {object} common.Resp[any]
// @Router /fs/form [put]
// @Router /v4/fs/form [put]
func FsForm(c *gin.Context) {
	path := c.GetHeader("File-Path")
	path, err := url.PathUnescape(path)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	asTask := c.GetHeader("As-Task") == "true"
	overwrite := c.GetHeader("Overwrite") != "false"
	user := c.MustGet(ctxkey.User).(*model.User)
	path, err = user.JoinPath(path)
	if err != nil {
		common.ErrorResp(c, err, 403)
		return
	}
	if !overwrite {
		if res, _ := fs.Get(c, path, &fs.GetArgs{NoLog: true}); res != nil {
			_, _ = utils.CopyWithBuffer(io.Discard, c.Request.Body)
			common.ErrorI18n(c, "fsmanage.file_exists", 403, "name", stdpath.Base(path))
			return
		}
	}
	storage, err := fs.GetStorage(path, &fs.GetStoragesArgs{})
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	if storage.Config().NoUpload {
		common.ErrorI18n(c, "errs.upload_not_supported", 405)
		return
	}
	file, err := c.FormFile("file")
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	f, err := file.Open()
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	defer f.Close()
	dir, name := stdpath.Split(path)
	h := make(map[*utils.HashType]string)
	if md5 := c.GetHeader("X-File-Md5"); md5 != "" {
		h[utils.MD5] = md5
	}
	if sha1 := c.GetHeader("X-File-Sha1"); sha1 != "" {
		h[utils.SHA1] = sha1
	}
	if sha256 := c.GetHeader("X-File-Sha256"); sha256 != "" {
		h[utils.SHA256] = sha256
	}
	mimetype := file.Header.Get("Content-Type")
	if len(mimetype) == 0 {
		mimetype = utils.GetMimeType(name)
	}
	s := stream.FileStream{
		Obj: &model.Object{
			Name:     name,
			Size:     file.Size,
			Modified: getLastModified(c),
			HashInfo: utils.NewHashInfoByMap(h),
		},
		Reader:       f,
		Mimetype:     mimetype,
		WebPutAsTask: asTask,
	}
	var t task.TaskExtensionInfo
	if asTask {
		s.Reader = struct {
			io.Reader
		}{f}
		t, err = fs.PutAsTask(c, dir, &s)
	} else {
		err = fs.PutDirectly(c, dir, &s, true)
	}
	if err != nil {
		common.ErrorResp(c, err, 500)
		return
	}
	if t == nil {
		common.SuccessResp(c)
		return
	}
	common.SuccessResp(c, gin.H{
		"task": getTaskInfo(c, t),
	})
}
