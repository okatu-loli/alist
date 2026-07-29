package handles

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/setting"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/alist-org/alist/v3/server/common"
	"github.com/gin-gonic/gin"
)

// Ping is a liveness check that always replies "pong".
// @Summary Ping
// @Description Liveness check; always replies "pong". Mounted at the top-level route (not under /api).
// @Tags public
// @Produce plain
// @Success 200 {string} string "pong"
// @Router /../ping [get]
func Ping(c *gin.Context) {
	c.String(200, "pong")
}

// Favicon redirects to the configured favicon URL.
// @Summary Favicon
// @Description Redirects to the site's configured favicon URL. Mounted at the top-level route (not under /api).
// @Tags public
// @Produce json
// @Success 302 {string} string "redirect to the favicon URL"
// @Router /../favicon.ico [get]
func Favicon(c *gin.Context) {
	c.Redirect(302, setting.GetStr(conf.Favicon))
}

// Robots returns the site's robots.txt content.
// @Summary robots.txt
// @Description Returns the configured robots.txt content. Mounted at the top-level route (not under /api).
// @Tags public
// @Produce plain
// @Success 200 {string} string "robots.txt content"
// @Router /../robots.txt [get]
func Robots(c *gin.Context) {
	c.String(200, setting.GetStr(conf.RobotsTxt))
}

// Plist decodes a base64-encoded signed link and serves an iOS OTA-install
// manifest (.plist) pointing at it.
// @Summary iOS OTA install manifest
// @Description Decodes the base64-encoded `{link}/{name}` payload in link_name and returns an Apple OTA-install .plist manifest referencing it. Mounted at the top-level route (not under /api).
// @Tags public
// @Produce xml
// @Param link_name path string true "base64-encoded \"link/name\" payload, with optional .plist suffix"
// @Success 200 {string} string "application/xml plist manifest"
// @Failure 400 {object} common.Resp[any] "malformed link_name"
// @Router /../i/{link_name} [get]
func Plist(c *gin.Context) {
	linkNameB64 := strings.TrimSuffix(c.Param("link_name"), ".plist")
	linkName, err := utils.SafeAtob(linkNameB64)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	linkNameSplit := strings.Split(linkName, "/")
	if len(linkNameSplit) != 2 {
		common.ErrorI18n(c, "plist.malformed_link", 400)
		return
	}
	linkEncode := linkNameSplit[0]
	linkStr, err := url.PathUnescape(linkEncode)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	link, err := url.Parse(linkStr)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	fullName := c.Param("name")
	Url := link.String()
	Url = strings.ReplaceAll(Url, "<", "[")
	Url = strings.ReplaceAll(Url, ">", "]")
	nameEncode := linkNameSplit[1]
	fullName, err = url.PathUnescape(nameEncode)
	if err != nil {
		common.ErrorResp(c, err, 400)
		return
	}
	name := fullName
	identifier := fmt.Sprintf("ci.nn.%s", url.PathEscape(fullName))
	sep := "@"
	if strings.Contains(fullName, sep) {
		ss := strings.Split(fullName, sep)
		name = strings.Join(ss[:len(ss)-1], sep)
		identifier = ss[len(ss)-1]
	}

	name = strings.ReplaceAll(name, "<", "[")
	name = strings.ReplaceAll(name, ">", "]")
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
    <dict>
        <key>items</key>
        <array>
            <dict>
                <key>assets</key>
                <array>
                    <dict>
                        <key>kind</key>
                        <string>software-package</string>
                        <key>url</key>
                        <string><![CDATA[%s]]></string>
                    </dict>
                </array>
                <key>metadata</key>
                <dict>
                    <key>bundle-identifier</key>
					<string>%s</string>
					<key>bundle-version</key>
                    <string>4.4</string>
                    <key>kind</key>
                    <string>software</string>
                    <key>title</key>
                    <string>%s</string>
                </dict>
            </dict>
        </array>
    </dict>
</plist>`, Url, identifier, name)
	c.Header("Content-Type", "application/xml;charset=utf-8")
	c.Status(200)
	_, _ = c.Writer.WriteString(plist)
}
