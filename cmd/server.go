package cmd

import (
	"context"
	"errors"
	"fmt"
	"github.com/alist-org/alist/v3/internal/audit"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/alist-org/alist/v3/cmd/flags"
	"github.com/alist-org/alist/v3/internal/bootstrap"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/internal/i18n"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/alist-org/alist/v3/server"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// ServerCmd represents the server command
var ServerCmd = &cobra.Command{
	Use:   "server",
	Short: "Start the server at the specified address",
	Long: `Start the server at the specified address
the address is defined in config file`,
	Run: func(cmd *cobra.Command, args []string) {
		Init()
		if conf.Conf.DelayedStart != 0 {
			utils.Log.Infof("delayed start for %d seconds", conf.Conf.DelayedStart)
			time.Sleep(time.Duration(conf.Conf.DelayedStart) * time.Second)
		}
		bootstrap.LoadPlugins()
		bootstrap.LoadStorages()
		bootstrap.InitTaskManager()
		bootstrap.InitOfflineDownloadManager()
		bootstrap.StartTokenGC()
		bootstrap.StartScheduledJobs()
		bootstrap.StartBackgroundServices()
		audit.Start()
		if !flags.Debug && !flags.Dev {
			gin.SetMode(gin.ReleaseMode)
		}
		r := gin.New()
		r.Use(gin.LoggerWithWriter(log.StandardLogger().Out), gin.RecoveryWithWriter(log.StandardLogger().Out))
		server.Init(r)
		var httpHandler http.Handler = r
		if conf.Conf.Scheme.EnableH2c {
			httpHandler = h2c.NewHandler(r, &http2.Server{})
		}
		var httpSrv, httpsSrv, unixSrv *http.Server
		if conf.Conf.Scheme.HttpPort != -1 {
			httpBase := fmt.Sprintf("%s:%d", conf.Conf.Scheme.Address, conf.Conf.Scheme.HttpPort)
			utils.Log.Info(i18n.Log("log.server_started_http", "addr", httpBase))
			httpSrv = &http.Server{Addr: httpBase, Handler: httpHandler}
			go func() {
				err := httpSrv.ListenAndServe()
				if err != nil && !errors.Is(err, http.ErrServerClosed) {
					utils.Log.Fatalf("failed to start http: %s", err.Error())
				}
			}()
		}
		if conf.Conf.Scheme.HttpsPort != -1 {
			httpsBase := fmt.Sprintf("%s:%d", conf.Conf.Scheme.Address, conf.Conf.Scheme.HttpsPort)
			utils.Log.Info(i18n.Log("log.server_started_https", "addr", httpsBase))
			httpsSrv = &http.Server{Addr: httpsBase, Handler: r}
			go func() {
				err := httpsSrv.ListenAndServeTLS(conf.Conf.Scheme.CertFile, conf.Conf.Scheme.KeyFile)
				if err != nil && !errors.Is(err, http.ErrServerClosed) {
					utils.Log.Fatalf("failed to start https: %s", err.Error())
				}
			}()
		}
		if conf.Conf.Scheme.UnixFile != "" {
			utils.Log.Info(i18n.Log("log.server_started_unix", "path", conf.Conf.Scheme.UnixFile))
			unixSrv = &http.Server{Handler: httpHandler}
			go func() {
				listener, err := net.Listen("unix", conf.Conf.Scheme.UnixFile)
				if err != nil {
					utils.Log.Fatalf("failed to listen unix: %+v", err)
				}
				// set socket file permission
				mode, err := strconv.ParseUint(conf.Conf.Scheme.UnixFilePerm, 8, 32)
				if err != nil {
					utils.Log.Errorf("failed to parse socket file permission: %+v", err)
				} else {
					err = os.Chmod(conf.Conf.Scheme.UnixFile, os.FileMode(mode))
					if err != nil {
						utils.Log.Errorf("failed to chmod socket file: %+v", err)
					}
				}
				err = unixSrv.Serve(listener)
				if err != nil && !errors.Is(err, http.ErrServerClosed) {
					utils.Log.Fatalf("failed to start unix: %s", err.Error())
				}
			}()
		}
		// Wait for interrupt signal to gracefully shutdown the server with
		// a timeout of 1 second.
		quit := make(chan os.Signal, 1)
		// kill (no param) default send syscanll.SIGTERM
		// kill -2 is syscall.SIGINT
		// kill -9 is syscall. SIGKILL but can"t be catch, so don't need add it
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
		<-quit
		utils.Log.Info(i18n.Log("log.server_shutdown"))
		Release()
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		var wg sync.WaitGroup
		if conf.Conf.Scheme.HttpPort != -1 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := httpSrv.Shutdown(ctx); err != nil {
					utils.Log.Fatal("HTTP server shutdown err: ", err)
				}
			}()
		}
		if conf.Conf.Scheme.HttpsPort != -1 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := httpsSrv.Shutdown(ctx); err != nil {
					utils.Log.Fatal("HTTPS server shutdown err: ", err)
				}
			}()
		}
		if conf.Conf.Scheme.UnixFile != "" {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := unixSrv.Shutdown(ctx); err != nil {
					utils.Log.Fatal("Unix server shutdown err: ", err)
				}
			}()
		}
		wg.Wait()
		utils.Log.Info(i18n.Log("log.server_exit"))
	},
}

func init() {
	RootCmd.AddCommand(ServerCmd)

	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// serverCmd.PersistentFlags().String("foo", "", "A help for foo")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	// serverCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
}

// OutAlistInit 暴露用于外部启动server的函数
func OutAlistInit() {
	var (
		cmd  *cobra.Command
		args []string
	)
	ServerCmd.Run(cmd, args)
}
