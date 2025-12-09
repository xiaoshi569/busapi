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
	log.Printf("🚀 [注册流程] 开始启动注册流程，目标注册数量: %d", count)

	if !atomic.CompareAndSwapInt32(&isRegistering, 0, 1) {
		log.Printf("⚠️ [注册流程] 注册进程已在运行，跳过")
		return fmt.Errorf("注册进程已在运行")
	}

	// 获取数据目录的绝对路径
	dataDirAbs, _ := filepath.Abs(DataDir)
	log.Printf("📁 [注册流程] 数据目录: %s", dataDirAbs)

	if err := os.MkdirAll(dataDirAbs, 0755); err != nil {
		atomic.StoreInt32(&isRegistering, 0)
		log.Printf("❌ [注册流程] 创建数据目录失败: %v", err)
		return fmt.Errorf("创建数据目录失败: %w", err)
	}

	// 使用配置的线程数
	threads := appConfig.Pool.RegisterThreads
	if threads <= 0 {
		threads = 1
	}
	log.Printf("🧵 [注册流程] 启动 %d 个注册线程", threads)

	for i := 0; i < threads; i++ {
		log.Printf("   ➜ 启动线程 %d", i+1)
		go NativeRegisterWorker(i+1, dataDirAbs)
	}

	// 监控进度
	go func() {
		log.Printf("👀 [注册流程] 启动进度监控器（每10秒检查一次）")
		checkCount := 0
		for {
			time.Sleep(10 * time.Second)
			checkCount++
			pool.Load(DataDir)
			currentCount := pool.TotalCount()
			targetCount := appConfig.Pool.TargetCount

			log.Printf("📊 [注册进度监控 #%d] 当前账号数: %d / %d (%.1f%%), 就绪: %d, 待刷新: %d",
				checkCount, currentCount, targetCount,
				float64(currentCount)/float64(targetCount)*100,
				pool.ReadyCount(), pool.PendingCount())

			if currentCount >= targetCount {
				log.Printf("✅ [注册流程] 已达到目标账号数: %d，停止注册", currentCount)
				atomic.StoreInt32(&isRegistering, 0)
				return
			}
		}
	}()

	log.Printf("✅ [注册流程] 注册流程启动成功")
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
	log.Printf("🔍 [号池维护] ========== 开始定期检查 ==========")
	log.Printf("📂 [号池维护] 重新加载账号数据: %s", DataDir)

	pool.Load(DataDir)

	readyCount := pool.ReadyCount()
	pendingCount := pool.PendingCount()
	totalCount := pool.TotalCount()
	targetCount := appConfig.Pool.TargetCount
	minCount := appConfig.Pool.MinCount

	log.Printf("📊 [号池维护] 账号池状态:")
	log.Printf("   • 就绪账号: %d", readyCount)
	log.Printf("   • 待刷新: %d", pendingCount)
	log.Printf("   • 总计: %d", totalCount)
	log.Printf("   • 目标数: %d (%.1f%%)", targetCount, float64(totalCount)/float64(targetCount)*100)
	log.Printf("   • 最小数: %d", minCount)

	if totalCount < targetCount {
		needCount := targetCount - totalCount
		log.Printf("⚠️ [号池维护] 账号数未达目标，缺口: %d 个", needCount)

		if totalCount < minCount {
			log.Printf("🚨 [号池维护] 账号数低于最小值 (%d < %d)，紧急启动注册", totalCount, minCount)
		}

		if err := startRegister(needCount); err != nil {
			log.Printf("❌ [号池维护] 启动注册失败: %v", err)
		}
	} else {
		log.Printf("✅ [号池维护] 账号数已达标 (%d/%d)", totalCount, targetCount)
	}

	log.Printf("✅ [号池维护] ========== 检查完成 ==========")
}
