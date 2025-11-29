package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ==================== 注册与刷新 ====================

var isRegistering int32

// 注册统计
type RegisterStats struct {
	Total     int       `json:"total"`
	Success   int       `json:"success"`
	Failed    int       `json:"failed"`
	LastError string    `json:"lastError"`
	UpdatedAt time.Time `json:"updatedAt"`
	mu        sync.RWMutex
}

var registerStats = &RegisterStats{}

func (s *RegisterStats) AddSuccess() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Total++
	s.Success++
	s.UpdatedAt = time.Now()
}

func (s *RegisterStats) AddFailed(err string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Total++
	s.Failed++
	s.LastError = err
	s.UpdatedAt = time.Now()
}

func (s *RegisterStats) Get() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return map[string]interface{}{
		"total":      s.Total,
		"success":    s.Success,
		"failed":     s.Failed,
		"last_error": s.LastError,
		"updated_at": s.UpdatedAt,
	}
}

// 注册结果
type RegisterResult struct {
	Success  bool   `json:"success"`
	Email    string `json:"email"`
	Error    string `json:"error"`
	NeedWait bool   `json:"needWait"`
}

func startRegister(count int) error {
	if !atomic.CompareAndSwapInt32(&isRegistering, 0, 1) {
		return fmt.Errorf("注册进程已在运行")
	}

	// 使用配置文件中的脚本路径
	scriptPath := appConfig.Pool.RegisterScript
	if scriptPath == "" {
		scriptPath = "./main.js"
	}

	// 转换为绝对路径
	if !filepath.IsAbs(scriptPath) {
		absPath, err := filepath.Abs(scriptPath)
		if err == nil {
			scriptPath = absPath
		}
	}

	// 检查脚本是否存在
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		atomic.StoreInt32(&isRegistering, 0)
		return fmt.Errorf("注册脚本不存在: %s", scriptPath)
	}

	// 获取数据目录的绝对路径
	dataDirAbs, _ := filepath.Abs(DataDir)

	// 使用配置的线程数
	threads := appConfig.Pool.RegisterThreads
	if threads <= 0 {
		threads = 1
	}

	log.Printf("📝 启动 %d 个注册线程，目标: %d 个，当前: %d 个", threads, appConfig.Pool.TargetCount, pool.TotalCount())

	for i := 0; i < threads; i++ {
		go registerWorker(i+1, scriptPath, dataDirAbs)
	}
	go func() {
		for {
			time.Sleep(10 * time.Second)
			pool.Load(DataDir)
			if pool.TotalCount() >= appConfig.Pool.TargetCount {
				log.Printf("✅ 已达到目标账号数: %d，停止注册", pool.TotalCount())
				atomic.StoreInt32(&isRegistering, 0)
				return
			}
		}
	}()

	return nil
}

func registerWorker(id int, scriptPath, dataDirAbs string) {
	// 错位启动，避免同时启动太多浏览器
	time.Sleep(time.Duration(id) * 3 * time.Second)

	for atomic.LoadInt32(&isRegistering) == 1 {
		// 检查是否已达到目标
		if pool.TotalCount() >= appConfig.Pool.TargetCount {
			return
		}

		log.Printf("[注册线程 %d] 启动注册任务", id)

		args := []string{scriptPath, "--threads", "1", "--data-dir", dataDirAbs, "--quiet"}
		if appConfig.Pool.RegisterHeadless {
			args = append(args, "--headless")
		}
		// 传递代理配置
		if Proxy != "" {
			args = append(args, "--proxy", Proxy)
		}

		cmd := exec.Command("node", args...)
		cmd.Dir = filepath.Dir(scriptPath)

		// 创建管道读取输出
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			log.Printf("[注册线程 %d] ⚠️ 创建管道失败: %v", id, err)
			time.Sleep(time.Second)
			continue
		}
		cmd.Stderr = os.Stderr

		if err := cmd.Start(); err != nil {
			log.Printf("[注册线程 %d] ⚠️ 启动失败: %v", id, err)
			time.Sleep(time.Second)
			continue
		}

		// 解析输出
		scanner := bufio.NewScanner(stdout)
		needWait := false
		for scanner.Scan() {
			line := scanner.Text()

			// 只解析结构化结果，不输出其他内容到控制台
			if strings.Contains(line, "@@REGISTER_RESULT@@") {
				start := strings.Index(line, "@@REGISTER_RESULT@@") + len("@@REGISTER_RESULT@@")
				end := strings.Index(line, "@@END@@")
				if end > start {
					jsonStr := line[start:end]
					var result RegisterResult
					if err := json.Unmarshal([]byte(jsonStr), &result); err == nil {
						if result.Success {
							registerStats.AddSuccess()
							log.Printf("[注册线程 %d] ✅ 注册成功: %s", id, result.Email)
						} else {
							registerStats.AddFailed(result.Error)
							log.Printf("[注册线程 %d] ❌ 注册失败: %s", id, result.Error)
							if result.NeedWait {
								needWait = true
							}
							// 连接/页面错误需要等待
							if strings.Contains(result.Error, "ERR_CONNECTION") ||
								strings.Contains(result.Error, "context was destroyed") ||
								strings.Contains(result.Error, "navigation") ||
								strings.Contains(result.Error, "timeout") ||
								strings.Contains(result.Error, "Cannot read properties") {
								needWait = true
							}
						}
					}
				}
			}
		}

		cmd.Wait()

		// 重新加载账号池
		pool.Load(DataDir)

		// 如果需要等待（频率限制或连接错误），延迟更长时间
		if needWait {
			waitTime := 10 + id*2 // 每个线程等待时间不同，避免同时重试
			log.Printf("[注册线程 %d] ⏳ 等待 %d 秒后重试...", id, waitTime)
			time.Sleep(time.Duration(waitTime) * time.Second)
		} else {
			// 短暂延迟后继续
			time.Sleep(3 * time.Second)
		}
	}
	log.Printf("[注册线程 %d] 停止", id)
}

func poolMaintainer() {
	interval := time.Duration(appConfig.Pool.CheckIntervalMinutes) * time.Minute
	if interval < time.Minute {
		interval = 30 * time.Minute
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	checkAndMaintainPool()

	for range ticker.C {
		checkAndMaintainPool()
	}
}

func checkAndMaintainPool() {
	pool.Load(DataDir)

	readyCount := pool.ReadyCount()
	pendingCount := pool.PendingCount()
	totalCount := pool.TotalCount()

	log.Printf("📊 号池检查: ready=%d, pending=%d, total=%d, 目标=%d, 最小=%d",
		readyCount, pendingCount, totalCount, appConfig.Pool.TargetCount, appConfig.Pool.MinCount)

	if totalCount < appConfig.Pool.TargetCount {
		needCount := appConfig.Pool.TargetCount - totalCount
		log.Printf("⚠️ 账号数未达目标，需要注册 %d 个", needCount)
		if err := startRegister(needCount); err != nil {
			log.Printf("❌ 启动注册失败: %v", err)
		}
	}
}
