package common

import (
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/op"
	"github.com/alist-org/alist/v3/pkg/utils"
)

func IsStorageSignEnabled(rawPath string) bool {
	storage := op.GetBalancedStorage(rawPath)
	return storage != nil && storage.GetStorage().EnableSign
}

// ShouldProxy TODO need optimize
// when should be proxy?
// 1. config.MustProxy()
// 2. storage.WebProxy
// 3. proxy_types
func ShouldProxy(storage driver.Driver, filename string) bool {
	if proxyDriver, ok := storage.(driver.ProxyDriver); ok {
		if proxyDriver.ShouldProxyDownloads() || storage.GetStorage().WebProxy {
			return true
		}
	} else if storage.Config().MustProxy() || storage.GetStorage().WebProxy {
		return true
	}
	if utils.SliceContains(conf.SlicesMap[conf.ProxyTypes], utils.Ext(filename)) {
		return true
	}
	return false
}
