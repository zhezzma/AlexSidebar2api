package check

import (
	"alexsidebar2api/common/config"
	logger "alexsidebar2api/common/loggger"
)

func CheckEnvVariable() {
	logger.SysLog("environment variable checking...")

	if config.ASCookie == "" {
		logger.FatalLog("环境变量 AS_COOKIE 未设置")
	}

	logger.SysLog("environment variable check passed.")
}
