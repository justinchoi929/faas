package main

import (
	"faas/internal/api"
	"faas/internal/registry"
	"github.com/gin-gonic/gin"
	"log"
	"net/http"
	"os"
)

func main() {
	// 读取环境变量配置
	workerdBin := os.Getenv("WORKERD_BIN")
	if workerdBin == "" {
		workerdBin = "workerd" // 默认：workerd 在 PATH 中
	}
	apiPort := os.Getenv("API_PORT")
	if apiPort == "" {
		apiPort = "8081" // 部署 API 端口
	}
	mainPort := os.Getenv("MAIN_PORT")
	if mainPort == "" {
		mainPort = "80" // 路由转发端口（子域名访问）
	}

	// 初始化注册表
	reg := registry.Default(workerdBin)
	log.Printf("storage dir: %s", reg.StorageDir) // 日志输出存储目录

	// 启动部署 API 服务（独立协程）
	ginEngine := gin.Default()
	apiGroup := ginEngine.Group("/api")
	//apiGroup.Use(api.AuthMiddleware()) // 鉴权中间件
	{
		apiGroup.POST("/deploy/:funcName", api.DeployHandler(reg))
		apiGroup.POST("/rollback/:funcName", api.RollbackHandler(reg))
	}

	go func() {
		log.Printf("deploy API running on :%s", apiPort)
		if err := ginEngine.Run(":" + apiPort); err != nil {
			log.Fatalf("api server failed: %v", err)
		}
	}()

	// 启动路由转发服务（主端口）
	log.Printf("router proxy running on :%s", mainPort)
	http.HandleFunc("/", api.ProxyHandler(reg))
	if err := http.ListenAndServe(":"+mainPort, nil); err != nil {
		log.Fatalf("proxy server failed: %v", err)
	}
}
