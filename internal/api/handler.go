package api

import (
	"faas/internal/registry"
	"faas/internal/util"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// DeployRequest 部署请求体
type DeployRequest struct {
	Runtime string            `json:"runtime" binding:"required,oneof=js"` // 仅支持 JS
	Code    string            `json:"code" binding:"required"`             // JS 源码
	EnvVars map[string]string `json:"env_vars"`                            // 环境变量（可选）
	Version string            `json:"version"`                             // 版本
	Alias   string            `json:"alias"`                               // 别名（可选）
}

// AuthMiddleware 鉴权中间件（硬编码 Token，可扩展为用户系统）
func AuthMiddleware() gin.HandlerFunc {
	//validToken := os.Getenv("FAAS_DEPLOY_TOKEN")
	//if validToken == "" {
	//	panic("FAAS_DEPLOY_TOKEN environment variable not set")
	//}

	validToken := "faasToken"
	return func(c *gin.Context) {
		token := c.GetHeader("X-Deploy-Token")
		if token != validToken {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or missing token"})
			c.Abort()
			return
		}
		c.Next()
	}
}

// DeployHandler 部署/更新函数接口（POST /api/deploy/:funcName）
func DeployHandler(reg *registry.Registry) gin.HandlerFunc {
	return func(c *gin.Context) {
		funcName := c.Param("funcName")
		var req DeployRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		// 若版本为空
		if req.Version == "" {
			req.Version = time.Now().Format("20060102150405")
		}

		// 构建函数元数据
		subdomain := fmt.Sprintf("%s.%s.func.local", req.Version, funcName)
		meta := &registry.FunctionMetadata{
			Name:      funcName,
			Subdomain: subdomain,
			Runtime:   req.Runtime,
			Code:      req.Code,
			EnvVars:   req.EnvVars,
			Version:   req.Version,
			Alias:     req.Alias,
		}

		// 注册/更新函数
		if err := reg.RegisterOrUpdate(meta); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		// 返回成功结果
		c.JSON(http.StatusOK, gin.H{
			"status":    "success",
			"funcName":  funcName,
			"subdomain": subdomain,
			"accessUrl": "http://" + subdomain,
			"version":   req.Version,
			"alias":     req.Alias,
		})
	}
}

// RollbackRequest 回滚请求体
type RollbackRequest struct {
	Alias   string `json:"alias"`                      // 要修改的别名（如latest）
	Version string `json:"version" binding:"required"` // 目标版本
}

// RollbackHandler 回滚接口（POST /api/rollback/:funcName）
func RollbackHandler(reg *registry.Registry) gin.HandlerFunc {
	return func(c *gin.Context) {
		funcName := c.Param("funcName")
		var req RollbackRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		if err := reg.Rollback(&req.Alias, funcName, req.Version); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"status":        "success",
			"funcName":      funcName,
			"alias":         req.Alias,
			"targetVersion": req.Version,
			"accessUrl":     fmt.Sprintf("http://%s.%s.func.local", req.Version, funcName),
		})
	}
}

type StopRequest struct {
	Version string `json:"version" binding:"required"` // 版本
}

// StopHandler 回滚接口（POST /api/stop/:funcName）
func StopHandler(reg *registry.Registry) gin.HandlerFunc {
	return func(c *gin.Context) {
		funcName := c.Param("funcName")
		var req StopRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := reg.StopFunction(funcName, req.Version); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"status":   "success",
			"funcName": funcName,
			"version":  req.Version,
			"message":  "function stopped successfully",
		})
	}
}

// DeleteFunctionHandler 删除整个函数接口（POST /api/delete/:funcName）
func DeleteFunctionHandler(reg *registry.Registry) gin.HandlerFunc {
	return func(c *gin.Context) {
		funcName := c.Param("funcName")

		if err := reg.DeleteFunction(funcName); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"status":   "success",
			"funcName": funcName,
			"message":  "function and all versions deleted successfully",
		})
	}
}

type DeleteVersionRequest struct {
	Version string `json:"version" binding:"required"` // 版本
}

// DeleteVersionHandler 删除函数特定版本接口（POST /api/delete/:funcName/version）
func DeleteVersionHandler(reg *registry.Registry) gin.HandlerFunc {
	return func(c *gin.Context) {
		funcName := c.Param("funcName")
		var req DeleteVersionRequest

		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		if err := reg.DeleteFunctionVersion(funcName, req.Version); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"status":   "success",
			"funcName": funcName,
			"version":  req.Version,
			"message":  "function version deleted successfully",
		})
	}
}

// ListVersionsHandler list 所有 version（GET /api/list/:funcName）
func ListVersionsHandler(reg *registry.Registry) gin.HandlerFunc {
	return func(c *gin.Context) {
		funcName := c.Param("funcName")
		var versions []string
		reg.Mu.RLock()
		for k, meta := range reg.VersionMap {
			parts := strings.SplitN(k, ":", 2)
			if len(parts) == 2 && parts[0] == funcName {
				versions = append(versions, meta.Version)
			}
		}
		reg.Mu.RUnlock()
		sort.Strings(versions)
		c.JSON(http.StatusOK, gin.H{
			"funcName": funcName,
			"versions": versions,
		})
	}
}

// ProxyHandler 路由转发处理器：解析子域名，转发请求到 workerd 进程
func ProxyHandler(reg *registry.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 提取子域名（如 foo.func.local）
		subdomain := r.Host
		if subdomain == "" {
			http.Error(w, "missing Host header", http.StatusBadRequest)
			return
		}

		// 查询函数元数据
		meta, exists := reg.GetBySubdomain(subdomain)
		if !exists {
			// 子域名未找到时，尝试通过别名查询
			meta, exists = reg.GetByAlias(subdomain)
			if !exists {
				// latest 情况
				meta, exists = reg.GetByName(strings.Split(subdomain, ".")[0])
				if !exists {
					http.Error(w, "function not found", http.StatusNotFound)
					return
				}
			}
		}

		// 检查进程状态并更新访问时间
		reg.Mu.Lock()
		if meta.Status == "" || meta.Status == "suspended" {
			// 唤醒进程：重新分配端口
			freePort, err := util.GetFreePort()
			if err != nil {
				reg.Mu.Unlock()
				http.Error(w, "failed to get free port", http.StatusInternalServerError)
				return
			}
			meta.Workerd.Port = freePort

			// 启动进程
			if err := reg.StartWorkerd(meta); err != nil {
				reg.Mu.Unlock()
				http.Error(w, "failed to wake up function", http.StatusInternalServerError)
				return
			}
			meta.Status = "running"
		}
		meta.LastAccessed = time.Now()
		reg.Mu.Unlock()

		// 转发请求到 workerd 进程（本地端口）
		targetUrl, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", meta.Workerd.Port))
		if err != nil {
			http.Error(w, "invalid target url", http.StatusInternalServerError)
			return
		}
		proxy := httputil.NewSingleHostReverseProxy(targetUrl)
		proxy.ServeHTTP(w, r)
	}
}
