package bootstrap

import (
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/internal/setting"
	"github.com/alist-org/alist/v3/internal/stream"
	"golang.org/x/time/rate"
)

func streamFilterNegative(limit int) (rate.Limit, int) {
	if limit < 0 {
		return rate.Inf, 0
	}
	return rate.Limit(limit) * 1024.0, limit * 1024
}

func initLimiter(limiter *stream.Limiter, s string) {
	clientDownLimit, burst := streamFilterNegative(setting.GetInt(s, -1))
	*limiter = stream.NewBurstLimiter(clientDownLimit, burst)
	op.RegisterSettingChangingCallback(func() {
		newLimit, newBurst := streamFilterNegative(setting.GetInt(s, -1))
		(*limiter).SetLimit(newLimit)
		(*limiter).SetBurst(newBurst)
	})
}

func InitStreamLimit() {
	initLimiter(&stream.ClientDownloadLimit, conf.StreamMaxClientDownloadSpeed)
	initLimiter(&stream.ClientUploadLimit, conf.StreamMaxClientUploadSpeed)
	initLimiter(&stream.ServerDownloadLimit, conf.StreamMaxServerDownloadSpeed)
	initLimiter(&stream.ServerUploadLimit, conf.StreamMaxServerUploadSpeed)
}
