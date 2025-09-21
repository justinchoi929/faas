package api

import (
	"net/http"
	"os"

	"faas/internal/registry" // 替换为实际模块路径
	"github.com/gin-gonic/gin"
)

// DeployRequest 部署请求体
type DeployRequest struct {
	Runtime string            `json:"runtime" binding:"required,oneof=js"` // 仅支持 JS
	Code    string            `json:"code" binding:"required"`             // JS 源码
	EnvVars map[string]string `json:"env_vars"`                            // 环境变量（可选）
	Version string            `json:"version"`                             // 版本（可选）
	Alias   string            `json:"alias"`                               // 别名（可选）
}

// AuthMiddleware 鉴权中间件（硬编码 Token，可扩展为用户系统）
func AuthMiddleware() gin.HandlerFunc {
	validToken := os.Getenv("FAAS_DEPLOY_TOKEN")
	if validToken == "" {
		panic("FAAS_DEPLOY_TOKEN environment variable not set")
	}

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

		// 构建函数元数据
		subdomain := funcName + ".func.local"
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
		})
	}
}
