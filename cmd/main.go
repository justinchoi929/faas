package main

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"

	"faas/internal/api"      // 替换为实际模块路径
	"faas/internal/registry" // 替换为实际模块路径

	"github.com/gin-gonic/gin"
)

// 路由转发处理器：解析子域名，转发请求到 workerd 进程
func proxyHandler(reg *registry.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 1. 提取子域名（如 foo.func.local）
		subdomain := r.Host
		if subdomain == "" {
			http.Error(w, "missing Host header", http.StatusBadRequest)
			return
		}

		// 2. 查询函数元数据
		meta, exists := reg.GetBySubdomain(subdomain)
		if !exists {
			http.Error(w, "function not found", http.StatusNotFound)
			return
		}

		// 3. 转发请求到 workerd 进程（本地端口）
		targetUrl, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", meta.Workerd.Port))
		if err != nil {
			http.Error(w, "invalid target url", http.StatusInternalServerError)
			return
		}
		proxy := httputil.NewSingleHostReverseProxy(targetUrl)
		proxy.ServeHTTP(w, r)
	}
}

func main() {
	// 1. 读取环境变量配置
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

	// 2. 初始化注册表
	reg := registry.Default(workerdBin)
	log.Printf("storage dir: %s", reg.StorageDir) // 日志输出存储目录

	// 3. 启动部署 API 服务（独立协程）
	ginEngine := gin.Default()
	apiGroup := ginEngine.Group("/api")
	//apiGroup.Use(api.AuthMiddleware()) // 鉴权中间件
	{
		apiGroup.POST("/deploy/:funcName", api.DeployHandler(reg))
	}

	go func() {
		log.Printf("deploy API running on :%s", apiPort)
		if err := ginEngine.Run(":" + apiPort); err != nil {
			log.Fatalf("api server failed: %v", err)
		}
	}()

	// 4. 启动路由转发服务（主端口）
	log.Printf("router proxy running on :%s", mainPort)
	http.HandleFunc("/", proxyHandler(reg))
	if err := http.ListenAndServe(":"+mainPort, nil); err != nil {
		log.Fatalf("proxy server failed: %v", err)
	}
}
