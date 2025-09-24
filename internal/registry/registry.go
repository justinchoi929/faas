package registry

import (
	"bytes"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// WorkerdConfig workerd 进程配置
type WorkerdConfig struct {
	Port     int    `json:"port"`
	ConfPath string `json:"conf_path"`
	CodePath string `json:"code_path"`
	LogPath  string `json:"log_path"`
	Pid      int    `gorm:"-" json:"pid"` // 忽略PID，不持久化
}

// FunctionMetadata 函数元数据
type FunctionMetadata struct {
	gorm.Model               // 内置字段：ID, CreatedAt, UpdatedAt, DeletedAt
	Name       string        `gorm:"index;not null" json:"name"` // 函数名
	Subdomain  string        `gorm:"uniqueIndex;not null" json:"subdomain"`
	Runtime    string        `gorm:"not null" json:"runtime"`
	Code       string        `gorm:"type:text;not null" json:"code"`         // 存储函数代码
	EnvVars    JSONMap       `gorm:"type:text;default:'{}'" json:"env_vars"` // 环境变量（JSON存储）
	Version    string        `gorm:"index;not null" json:"version"`          // 版本号（必填）
	Alias      string        `json:"alias"`
	Workerd    WorkerdConfig `gorm:"type:json;default:'{}'" json:"workerd"` // 嵌套结构体，会被展开为WorkerdPort, WorkerdConfPath等字段
}

// Registry 函数注册表（单例）
type Registry struct {
	funcs        map[string]*FunctionMetadata // 函数名 -> 元数据
	subdomainMap map[string]string            // 子域名 -> 函数名
	mu           sync.RWMutex                 // 并发安全锁
	StorageDir   string                       // 存储目录
	workerdBin   string                       // workerd 二进制路径
	versionMap   map[string]*FunctionMetadata // funcName:version -> 元数据（唯一标识版本）
	aliasMap     map[string]string            // funcName:alias -> version（别名指向版本）
	db           *gorm.DB                     // 数据库连接
}

var defaultRegistry *Registry

// Default 获取单例注册表
func Default(workerdBin string) *Registry {
	if defaultRegistry == nil {
		// 初始化数据库
		db, err := gorm.Open(sqlite.Open(filepath.Join(getStorageDir(), "faas.db")), &gorm.Config{})
		if err != nil {
			panic(fmt.Sprintf("failed to connect database: %v", err))
		}

		// 自动迁移表结构
		if err := db.AutoMigrate(&FunctionMetadata{}); err != nil {
			panic(fmt.Sprintf("failed to migrate database: %v", err))
		}

		// 创建注册表实例
		defaultRegistry = &Registry{
			funcs:        make(map[string]*FunctionMetadata),
			subdomainMap: make(map[string]string),
			StorageDir:   getStorageDir(),
			workerdBin:   workerdBin,
			versionMap:   make(map[string]*FunctionMetadata),
			aliasMap:     make(map[string]string),
			db:           db,
		}

		// 从数据库加载已保存的函数
		err = defaultRegistry.loadFromDB()
		if err != nil {
			_ = fmt.Errorf("load from DB failed: %w", err)
		}
	}
	return defaultRegistry
}

// 生成 workerd 配置与代码文件
func (r *Registry) generateWorkerdFiles(meta *FunctionMetadata) error {
	// 生成函数代码文件（如 storage/foo.js）
	codeFile := fmt.Sprintf("%s.js", meta.Name)
	codePath := filepath.Join(r.StorageDir, codeFile)
	if err := os.WriteFile(codePath, []byte(meta.Code), 0644); err != nil {
		return fmt.Errorf("write code: %w", err)
	}
	meta.Workerd.CodePath = codePath

	// 生成配置文件（注意 embed 必须是相对路径）
	confPath := filepath.Join(r.StorageDir, fmt.Sprintf("%s.capnp", meta.Name))
	confContent := fmt.Sprintf(`
using Workerd = import "/workerd/workerd.capnp";

const config :Workerd.Config = (
  services = [
    (
      name = "%s",
      worker = (
        serviceWorkerScript = embed "%s",
        compatibilityDate = "2024-05-01"
      )
    )
  ],
  sockets = [
    (
      name = "http",
      address = "127.0.0.1:%d",
      http = (),
      service = "%s"
    )
  ]
);
`, meta.Name, codeFile, meta.Workerd.Port, meta.Name)

	if err := os.WriteFile(confPath, []byte(confContent), 0644); err != nil {
		return fmt.Errorf("write conf: %w", err)
	}
	meta.Workerd.ConfPath = confPath

	// 生成日志文件路径
	logPath := filepath.Join(r.StorageDir, fmt.Sprintf("%s.log", meta.Name))
	meta.Workerd.LogPath = logPath
	return nil
}

// 启动/停止 workerd 进程
func (r *Registry) startWorkerd(meta *FunctionMetadata) error {
	// 生成配置/代码文件
	if err := r.generateWorkerdFiles(meta); err != nil {
		return err
	}

	// 启动 workerd 进程（命令：workerd serve 配置文件）
	cmd := exec.Command(r.workerdBin, "serve", meta.Workerd.ConfPath)
	fmt.Printf("[DEBUG] Running: %s serve %s\n", r.workerdBin, meta.Workerd.ConfPath)
	// 重定向日志到文件
	logFile, err := os.OpenFile(meta.Workerd.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	// 捕获 stderr
	var stderrBuf bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stdout, logFile)
	cmd.Stderr = io.MultiWriter(os.Stderr, logFile)

	// 启动进程并记录 PID
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start workerd: %v, stderr: %s", err, stderrBuf.String())
	}
	meta.Workerd.Pid = cmd.Process.Pid

	// 等待端口监听成功
	if err := waitPortListening("127.0.0.1", meta.Workerd.Port); err != nil {
		cmd.Process.Kill() // 启动失败，清理进程
		return fmt.Errorf("wait port: %v, stderr: %s", err, stderrBuf.String())
	}
	return nil
}

func (r *Registry) stopWorkerd(meta *FunctionMetadata) error {
	if meta.Workerd.Pid == 0 {
		return nil // 进程未启动
	}

	process, err := os.FindProcess(meta.Workerd.Pid)
	if err != nil {
		return fmt.Errorf("find process: %w", err)
	}

	// 检查进程是否还活着
	err = process.Signal(syscall.Signal(0))
	if err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			// 进程已经结束，不需要停止
			meta.Workerd.Pid = 0
			return nil
		}
		return fmt.Errorf("check process: %w", err)
	}

	// 发送终止信号
	if err := process.Signal(syscall.SIGTERM); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			// 已经结束
			meta.Workerd.Pid = 0
			return nil
		}
		return fmt.Errorf("send signal: %w", err)
	}

	// 等待进程退出
	_, err = process.Wait()
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("wait exit: %w", err)
	}

	meta.Workerd.Pid = 0
	return nil
}

// RegisterOrUpdate 注册/更新函数
func (r *Registry) RegisterOrUpdate(meta *FunctionMetadata) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// 更新 latest 指向
	r.aliasMap[fmt.Sprintf("%s:latest", meta.Name)] = meta.Version

	// 生成唯一标识
	versionKey := fmt.Sprintf("%s:%s", meta.Name, meta.Version)

	// 分配空闲端口
	freePort, err := getFreePort()
	if err != nil {
		return fmt.Errorf("get free port: %w", err)
	}
	meta.Workerd.Port = freePort

	// 启动新版本进程（不影响旧版本）
	if err := r.startWorkerd(meta); err != nil {
		return fmt.Errorf("start new version: %w", err)
	}

	// 停止旧函数（更新场景）
	//if oldMeta, exists := r.funcs[meta.Name]; exists {
	//	if err := r.stopWorkerd(oldMeta); err != nil {
	//		return fmt.Errorf("stop old function: %w", err)
	//	}
	//	delete(r.subdomainMap, oldMeta.Subdomain)
	//	// 保留旧记录主键和域名
	//	meta.ID = oldMeta.ID
	//	meta.CreatedAt = oldMeta.CreatedAt
	//	meta.Subdomain = oldMeta.Subdomain
	//}

	// 处理别名
	if meta.Alias != "" {
		aliasKey := fmt.Sprintf("%s:%s", meta.Name, meta.Alias)
		oldVersion, exists := r.aliasMap[aliasKey]
		// 移除旧别名的子域名映射
		if exists {
			oldMetaKey := fmt.Sprintf("%s:%s", meta.Name, oldVersion)
			if oldMeta, ok := r.versionMap[oldMetaKey]; ok {
				delete(r.subdomainMap, oldMeta.Subdomain)
			}
		}

		// 注册新别名映射
		r.aliasMap[aliasKey] = meta.Version
		aliasSubdomain := r.generateAliasSubdomain(meta.Name, meta.Alias)
		r.subdomainMap[aliasSubdomain] = versionKey // 别名子域名指向版本
	}

	// 存储元数据
	r.funcs[meta.Name] = meta
	meta.UpdatedAt = time.Now()
	if err := r.db.Save(meta).Error; err != nil { // gorm.Save会自动判断新增/更新
		return fmt.Errorf("save to db: %w", err)
	}

	// 更新内存映射
	r.funcs[meta.Name] = meta
	r.versionMap[versionKey] = meta
	r.subdomainMap[meta.Subdomain] = versionKey

	return nil
}

// Rollback 别名回滚
func (r *Registry) Rollback(alias *string, funcName, targetVersion string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	targetKey := fmt.Sprintf("%s:%s", funcName, targetVersion)
	targetMeta, exists := r.versionMap[targetKey]
	if !exists {
		return errors.New("target version not found")
	}

	// 若目标版本进程未启动，尝试启动
	if targetMeta.Workerd.Pid == 0 {
		if err := r.startWorkerd(targetMeta); err != nil {
			return fmt.Errorf("start target version: %w", err)
		}
	}

	// 后续别名更新逻辑
	if *alias != "" {
		aliasKey := fmt.Sprintf("%s:%s", funcName, *alias)
		oldVersion, exists := r.aliasMap[aliasKey]
		if exists {
			oldMetaKey := fmt.Sprintf("%s:%s", funcName, oldVersion)
			if _, ok := r.versionMap[oldMetaKey]; ok {
				delete(r.subdomainMap, r.generateAliasSubdomain(funcName, *alias))
			}
		}
		r.aliasMap[aliasKey] = targetVersion
	} else {
		*alias = targetMeta.Alias
	}
	r.funcs[funcName] = targetMeta
	aliasSubdomain := r.generateAliasSubdomain(funcName, *alias)
	r.subdomainMap[aliasSubdomain] = targetKey

	return nil
}

// 从数据库加载函数元数据并启动进程
func (r *Registry) loadFromDB() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var metas []*FunctionMetadata
	if err := r.db.Where("deleted_at IS NULL").Find(&metas).Error; // 关键修复：排除已删除记录
	err != nil {
		return fmt.Errorf("load from db: %w", err)
	}
	latestVersions := make(map[string]string)

	for _, meta := range metas {
		// 重置端口（避免重启后端口冲突）
		meta.Workerd.Port = 0
		freePort, err := getFreePort()
		if err != nil {
			fmt.Printf("failed to get free port for function %s: %v\n", meta.Name, err)
			continue
		}
		meta.Workerd.Port = freePort
		// 重新生成配置文件并启动进程
		if err := r.startWorkerd(meta); err != nil {
			// 记录启动失败的函数，但继续加载其他函数
			fmt.Printf("failed to restart function %s: %v\n", meta.Name, err)
			continue
		}
		// 重建 versionMap
		versionKey := fmt.Sprintf("%s:%s", meta.Name, meta.Version)
		r.versionMap[versionKey] = meta

		// 重建 subdomainMap
		r.subdomainMap[meta.Subdomain] = versionKey

		// 重建 aliasMap
		if meta.Alias != "" {
			aliasKey := fmt.Sprintf("%s:%s", meta.Name, meta.Alias)
			r.aliasMap[aliasKey] = meta.Version
			aliasSubdomain := r.generateAliasSubdomain(meta.Name, meta.Alias)
			r.subdomainMap[aliasSubdomain] = versionKey
		}

		// 重建 funcs 映射
		if existingMeta, exists := r.funcs[meta.Name]; !exists || meta.UpdatedAt.After(existingMeta.UpdatedAt) {
			r.funcs[meta.Name] = meta
			latestVersions[meta.Name] = meta.Version // 记录最新版本
		}

		r.db.Save(meta)
	}

	for funcName, latestVersion := range latestVersions {
		latestAliasKey := fmt.Sprintf("%s:latest", funcName)
		r.aliasMap[latestAliasKey] = latestVersion

		// 重建 latest 别名的子域名映射
		latestSubdomain := r.generateAliasSubdomain(funcName, "latest")
		latestVersionKey := fmt.Sprintf("%s:%s", funcName, latestVersion)
		if _, exists := r.versionMap[latestVersionKey]; exists {
			r.subdomainMap[latestSubdomain] = latestVersionKey
		}
	}

	fmt.Printf("loaded %d functions from database\n", len(r.funcs))
	return nil
}

func (r *Registry) DeleteVersion(funcName, version string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	versionKey := fmt.Sprintf("%s:%s", funcName, version)
	meta, exists := r.versionMap[versionKey]
	if !exists {
		return errors.New("version not found")
	}

	// 停止进程
	if err := r.stopWorkerd(meta); err != nil {
		return fmt.Errorf("stop version: %w", err)
	}

	// 从数据库删除
	if err := r.db.Where("name = ? AND version = ?", funcName, version).Delete(&FunctionMetadata{}).Error; err != nil {
		return fmt.Errorf("delete from db: %w", err)
	}

	// 清理映射
	delete(r.versionMap, versionKey)
	delete(r.subdomainMap, meta.Subdomain)

	// 清理别名
	for aliasKey, v := range r.aliasMap {
		if v == version && strings.HasPrefix(aliasKey, funcName+":") {
			aliasSubdomain := r.generateAliasSubdomain(funcName, strings.TrimPrefix(aliasKey, funcName+":"))
			delete(r.subdomainMap, aliasSubdomain)
			delete(r.aliasMap, aliasKey)
		}
	}

	return nil
}

// 辅助方法：查询函数
func (r *Registry) GetBySubdomain(subdomain string) (*FunctionMetadata, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	versionKey, exists := r.subdomainMap[subdomain]
	if !exists {
		return nil, false
	}
	meta, exists := r.versionMap[versionKey]
	return meta, exists
}

func (r *Registry) GetByName(funcName string) (*FunctionMetadata, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	meta, exists := r.funcs[funcName]
	return meta, exists
}

func (r *Registry) GetByVersion(funcName, version string) (*FunctionMetadata, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	key := fmt.Sprintf("%s:%s", funcName, version)
	meta, exists := r.versionMap[key]
	return meta, exists
}

func (r *Registry) GetByAlias(subdomain string) (*FunctionMetadata, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var alias string
	var funcName string
	parts := strings.Split(subdomain, ".")
	if len(parts) >= 2 {
		alias = parts[0]
		funcName = parts[1]
	} else {
		return nil, false
	}
	version, ok := r.aliasMap[fmt.Sprintf("%s:%s", funcName, alias)]
	if !ok {
		return nil, false
	}
	return r.GetByVersion(funcName, version)
}

// 生成版本专属子域名（如 7cc187.foo.func.local）
func (r *Registry) generateVersionSubdomain(funcName, version string) string {
	return fmt.Sprintf("%s.%s.func.local", version, funcName)
}

// 生成别名子域名（如 latest.foo.func.local）
func (r *Registry) generateAliasSubdomain(funcName, alias string) string {
	return fmt.Sprintf("%s.%s.func.local", alias, funcName)
}

// 实现gorm.Valuer接口，将WorkerdConfig转换为JSON字符串
func (w WorkerdConfig) Value() (driver.Value, error) {
	return json.Marshal(w)
}

// 实现gorm.Scanner接口，从JSON字符串解析为WorkerdConfig
func (w *WorkerdConfig) Scan(value interface{}) error {
	b, ok := value.([]byte)
	if !ok {
		return errors.New("type assertion to []byte failed")
	}
	return json.Unmarshal(b, &w)
}

// 自定义 JSON 类型做映射
type JSONMap map[string]string

func (m JSONMap) Value() (driver.Value, error) {
	if m == nil {
		return "{}", nil
	}
	return json.Marshal(m)
}

func (m *JSONMap) Scan(value interface{}) error {
	if value == nil {
		*m = make(JSONMap)
		return nil
	}
	var data []byte
	switch v := value.(type) {
	case string:
		data = []byte(v)
	case []byte:
		data = v
	default:
		return fmt.Errorf("unsupported type: %T", value)
	}
	return json.Unmarshal(data, m)
}
