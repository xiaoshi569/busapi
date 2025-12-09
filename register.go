package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
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
	log.Printf("🚀 [注册流程] 开始启动，目标注册数量: %d", count)

	if !atomic.CompareAndSwapInt32(&isRegistering, 0, 1) {
		return fmt.Errorf("注册进程已在运行")
	}

	// 获取数据目录的绝对路径
	dataDirAbs, _ := filepath.Abs(DataDir)

	if err := os.MkdirAll(dataDirAbs, 0755); err != nil {
		atomic.StoreInt32(&isRegistering, 0)
		return fmt.Errorf("创建数据目录失败: %w", err)
	}

	// 使用配置的线程数
	threads := appConfig.Pool.RegisterThreads
	if threads <= 0 {
		threads = 1
	}

	for i := 0; i < threads; i++ {
		go NativeRegisterWorker(i+1, dataDirAbs)
	}

	// 监控进度
	go func() {
		checkCount := 0
		for {
			time.Sleep(10 * time.Second)
			checkCount++
			pool.Load(DataDir)
			currentCount := pool.TotalCount()
			targetCount := appConfig.Pool.TargetCount

			if currentCount >= targetCount {
				atomic.StoreInt32(&isRegistering, 0)
				return
			}
		}
	}()

	return nil
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

	totalCount := pool.TotalCount()
	targetCount := appConfig.Pool.TargetCount
	minCount := appConfig.Pool.MinCount

	if totalCount < targetCount {
		needCount := targetCount - totalCount

		if totalCount < minCount {
			log.Printf("⚠️ 账号低于最小值 (%d < %d)，启动注册", totalCount, minCount)
		}

		if err := startRegister(needCount); err != nil {
			log.Printf("❌ 启动注册失败: %v", err)
		}
	}
}
