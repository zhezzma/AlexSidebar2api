// @title KILO-AI-2API
// @version 1.0.0
// @description KILO-AI-2API
// @BasePath
package main

import (
	"alexsidebar2api/check"
	"alexsidebar2api/common"
	"alexsidebar2api/common/config"
	logger "alexsidebar2api/common/loggger"
	"alexsidebar2api/job"
	"alexsidebar2api/middleware"
	"alexsidebar2api/model"
	"alexsidebar2api/router"
	"fmt"
	"os"
	"strconv"

	"github.com/gin-gonic/gin"
)

//var buildFS embed.FS

func main() {
	logger.SetupLogger()
	logger.SysLog(fmt.Sprintf("alexsidebar2api %s starting...", common.Version))

	check.CheckEnvVariable()

	if os.Getenv("GIN_MODE") != "debug" {
		gin.SetMode(gin.ReleaseMode)
	}

	var err error

	model.InitTokenEncoders()
	_, err = config.InitASCookies()
	if err != nil {
		logger.FatalLog(err)
	}

	server := gin.New()
	server.Use(gin.Recovery())
	server.Use(middleware.RequestId())
	middleware.SetUpLogger(server)

	// 设置API路由
	router.SetApiRouter(server)
	// 设置前端路由
	//router.SetWebRouter(server, buildFS)

	var port = os.Getenv("PORT")
	if port == "" {
		port = strconv.Itoa(*common.Port)
	}

	if config.DebugEnabled {
		logger.SysLog("running in DEBUG mode.")
	}

	logger.SysLog("alexsidebar2api start success. enjoy it! ^_^\n")
	go job.UpdateCookieTokenTask()

	err = server.Run(":" + port)

	if err != nil {
		logger.FatalLog("failed to start HTTP server: " + err.Error())
	}
}
