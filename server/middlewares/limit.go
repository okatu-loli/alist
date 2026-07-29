package middlewares

import (
	"io"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/setting"
	"github.com/alist-org/alist/v3/internal/stream"
	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

func MaxAllowed(n int) gin.HandlerFunc {
	sem := make(chan struct{}, n)
	acquire := func() { sem <- struct{}{} }
	release := func() { <-sem }
	return func(c *gin.Context) {
		acquire()
		defer release()
		c.Next()
	}
}

func UploadRateLimiter(limiter stream.Limiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = &stream.RateLimitReader{
			Reader:  c.Request.Body,
			Limiter: limiter,
			Ctx:     c,
		}
		c.Next()
	}
}

type ResponseWriterWrapper struct {
	gin.ResponseWriter
	WrapWriter io.Writer
}

func (w *ResponseWriterWrapper) Write(p []byte) (n int, err error) {
	return w.WrapWriter.Write(p)
}

func DownloadRateLimiter(limiter stream.Limiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Writer = &ResponseWriterWrapper{
			ResponseWriter: c.Writer,
			WrapWriter: &stream.RateLimitWriter{
				Writer:  c.Writer,
				Limiter: limiter,
				Ctx:     c,
			},
		}
		c.Next()
	}
}

// PerConnDownloadRateLimiter caps the bandwidth of EACH download connection
// independently, unlike DownloadRateLimiter whose stream.Limiter is a single
// bucket shared across all connections. The cap (conf.StreamMaxConnection
// DownloadSpeed, KiB/s) is read fresh on every request so an admin change takes
// effect without a restart; a non-positive value (the default) means unlimited
// and the wrapper is skipped entirely. When both limiters are active they
// compose: a connection is bounded by the smaller of its own cap and its share
// of the global bucket.
func PerConnDownloadRateLimiter() gin.HandlerFunc {
	return func(c *gin.Context) {
		if kib := setting.GetInt(conf.StreamMaxConnectionDownloadSpeed, -1); kib > 0 {
			limiter := stream.NewBurstLimiter(rate.Limit(kib)*1024, kib*1024)
			c.Writer = &ResponseWriterWrapper{
				ResponseWriter: c.Writer,
				WrapWriter: &stream.RateLimitWriter{
					Writer:  c.Writer,
					Limiter: limiter,
					Ctx:     c,
				},
			}
		}
		c.Next()
	}
}
