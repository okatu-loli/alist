package data

import (
	"regexp"
	"testing"

	"github.com/alist-org/alist/v3/internal/conf"
)

// 豁免清单：允许默认值里出现外部 URL 的设置键，每项必须给理由。
// 新增豁免 = 隐私承诺的例外，需在 rollout Step 2a 治理条款下评审。
var thirdPartyExempt = map[string]string{
	"announcement":     "默认公告里的项目主页链接：纯文本展示，浏览器不会自动加载",
	"aria2_uri":        "aria2 JSON-RPC 端点默认 http://localhost:6800/jsonrpc：指向管理员本机的 aria2 守护进程，非第三方域名，仅后端出站调用",
	"qbittorrent_url":  "qBittorrent WebUI 默认 http://admin:adminadmin@localhost:8080/：指向管理员本机的 qBittorrent 守护进程，非第三方域名，仅后端出站调用",
	"transmission_uri": "Transmission RPC 端点默认 http://localhost:9091/transmission/rpc：指向管理员本机的 Transmission 守护进程，非第三方域名，仅后端出站调用",
	"meilisearch_host": "Meilisearch 服务默认 http://localhost:7700：指向管理员本机/自托管的 Meilisearch 实例，非第三方域名，仅后端出站调用",
}

var urlRe = regexp.MustCompile(`(?i)(?:^|[^/\w])(?:https?:)?//[a-z0-9][a-z0-9.-]+`)

// 隐私基线（rollout Step 2a）：默认设置种子不得引用第三方域名——
// logo、封面、脚本等资源一律本站提供，第三方只能是用户自己填进去的。
func TestDefaultSettingsContainNoThirdPartyDomains(t *testing.T) {
	// InitialSettings() reads conf.Conf.Tasks.*.Workers (setting.go:175-176); this
	// package has no TestMain/bootstrap, so conf.Conf is nil unless a real server
	// startup ran first. Stub it so the call doesn't panic before we ever reach the
	// domain assertion below — the worker counts themselves aren't under test here.
	oldConf := conf.Conf
	conf.Conf = &conf.Config{}
	t.Cleanup(func() { conf.Conf = oldConf })

	for _, item := range InitialSettings() {
		if _, ok := thirdPartyExempt[item.Key]; ok {
			continue
		}
		for _, v := range []string{item.Value, item.PreDefault} {
			if m := urlRe.FindString(v); m != "" {
				t.Errorf("setting %q default references external URL %q — serve it locally or add a justified exemption", item.Key, m)
			}
		}
	}
}
