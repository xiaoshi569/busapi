package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"mime"
	"mime/quotedprintable"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"encoding/base64"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

var (
	RegisterDebug bool
	RegisterOnce  bool
	firstNames    = []string{"John", "Jane", "Michael", "Sarah", "David", "Emily", "Robert", "Lisa", "James", "Emma"}
	lastNames     = []string{"Smith", "Johnson", "Williams", "Brown", "Jones", "Garcia", "Miller", "Davis", "Wilson", "Taylor"}
	commonWords   = map[string]bool{
		"VERIFY": true, "GOOGLE": true, "UPDATE": true, "MOBILE": true, "DEVICE": true,
		"SUBMIT": true, "RESEND": true, "CANCEL": true, "DELETE": true, "REMOVE": true,
		"SEARCH": true, "VIDEOS": true, "IMAGES": true, "GMAIL": true, "EMAIL": true,
		"ACCOUNT": true, "CHROME": true,
		// 邮件技术词汇
		"ESMTPS": true, "ESMTP": true, "SMTP": true, "IMAPS": true, "IMAP": true,
		"STARTTLS": true, "EHLO": true, "HELO": true, "RCPT": true, "SENDER": true,
		"HEADER": true, "FOOTER": true, "BORDER": true, "CENTER": true, "BUTTON": true,
		"MAILTO": true, "DOMAIN": true, "SERVER": true, "CLIENT": true, "HTTPS": true,
	}
)

// TempEmailResponse 临时邮箱响应
type TempEmailResponse struct {
	Email string `json:"email"`
	Data  struct {
		Email string `json:"email"`
	} `json:"data"`
}

// EmailListResponse 邮件列表响应
type EmailListResponse struct {
	Success bool `json:"success"`
	Data    struct {
		Emails []EmailContent `json:"emails"`
	} `json:"data"`
}

// EmailContent 邮件内容
type EmailContent struct {
	Subject string `json:"subject"`
	Content string `json:"content"`
}

// BrowserRegisterResult 注册结果
type BrowserRegisterResult struct {
	Success       bool
	Email         string
	FullName      string
	Authorization string
	Cookies       []Cookie
	ConfigID      string
	CSESIDX       string
	Error         error
}

// generateRandomName 生成随机全名
func generateRandomName() string {
	return firstNames[rand.Intn(len(firstNames))] + " " + lastNames[rand.Intn(len(lastNames))]
}

// TempMailProvider 临时邮箱提供商
type TempMailProvider struct {
	Name        string
	GenerateURL string
	CheckURL    string
	Headers     map[string]string
}

// 支持的临时邮箱提供商列表
var tempMailProviders = []TempMailProvider{
	{
		Name:        "chatgpt.org.uk",
		GenerateURL: "https://mail.chatgpt.org.uk/api/generate-email",
		CheckURL:    "https://mail.chatgpt.org.uk/api/emails?email=%s",
		Headers: map[string]string{
			"User-Agent": "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36",
			"Referer":    "https://mail.chatgpt.org.uk",
		},
	},
	// 备用邮箱服务可以在这里添加
}

// 随机字符集
var randomChars = []rune("abcdefghijklmnopqrstuvwxyz0123456789")

// generateRandomString 生成指定长度的随机字符串
func generateRandomString(length int) string {
	b := make([]rune, length)
	for i := range b {
		b[i] = randomChars[rand.Intn(len(randomChars))]
	}
	return string(b)
}

// generateCustomDomainEmail 生成自定义域名的随机邮箱
func generateCustomDomainEmail(domain string) string {
	prefix := generateRandomString(8 + rand.Intn(5)) // 8-12位随机前缀
	return prefix + "@" + domain
}

// isQQImapConfigured 检查是否配置了IMAP邮箱（支持任何IMAP服务：Gmail, QQ, 163等）
func isQQImapConfigured() bool {
	return appConfig.Email.RegisterDomain != "" &&
		appConfig.Email.QQImap.Address != "" &&
		appConfig.Email.QQImap.AuthCode != ""
}

func getTemporaryEmail() (string, error) {
	log.Printf("📧 [临时邮箱] 开始获取临时邮箱...")

	// 优先使用自定义域名（IMAP邮箱转发方案）
	if isQQImapConfigured() {
		log.Printf("✅ [临时邮箱] 检测到IMAP邮箱配置，使用自定义域名")
		email := generateCustomDomainEmail(appConfig.Email.RegisterDomain)
		log.Printf("✅ [临时邮箱] 生成自定义域名邮箱: %s (转发到 %s)", email, appConfig.Email.QQImap.Address)
		return email, nil
	}

	// 回退到临时邮箱服务
	log.Printf("🔄 [临时邮箱] 使用临时邮箱服务，共 %d 个提供商", len(tempMailProviders))
	var lastErr error
	for i, provider := range tempMailProviders {
		log.Printf("🔍 [临时邮箱] 尝试提供商 %d/%d: %s", i+1, len(tempMailProviders), provider.Name)
		email, err := getEmailFromProvider(provider)
		if err != nil {
			lastErr = err
			log.Printf("❌ [临时邮箱] 提供商 %s 失败: %v，尝试下一个", provider.Name, err)
			continue
		}
		log.Printf("✅ [临时邮箱] 从 %s 获取到邮箱: %s", provider.Name, email)
		return email, nil
	}

	log.Printf("❌ [临时邮箱] 所有提供商均失败")
	return "", fmt.Errorf("所有临时邮箱服务均失败: %v", lastErr)
}

func getEmailFromProvider(provider TempMailProvider) (string, error) {
	log.Printf("   🌐 请求 %s API: %s", provider.Name, provider.GenerateURL)
	req, _ := http.NewRequest("GET", provider.GenerateURL, nil)
	for k, v := range provider.Headers {
		req.Header.Set(k, v)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("   ❌ HTTP请求失败: %v", err)
		return "", fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Printf("   ❌ HTTP状态码: %d", resp.StatusCode)
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	body, _ := readResponseBody(resp)
	log.Printf("   📥 响应大小: %d 字节", len(body))
	var result TempEmailResponse
	if err := json.Unmarshal(body, &result); err != nil {
		log.Printf("   ❌ JSON解析失败: %v", err)
		return "", fmt.Errorf("解析响应失败: %w", err)
	}

	email := result.Email
	if email == "" {
		email = result.Data.Email
	}
	if email == "" {
		log.Printf("   ❌ 响应中未包含邮箱地址")
		return "", fmt.Errorf("返回的邮箱为空")
	}
	log.Printf("   ✅ 解析到邮箱: %s", email)
	return email, nil
}

// ==================== IMAP邮箱读取（支持Gmail/QQ/163等） ====================

// testQQImapConnection 测试IMAP邮箱连接
func testQQImapConnection() {
	cfg := appConfig.Email.QQImap
	if cfg.Address == "" || cfg.AuthCode == "" {
		log.Println("❌ IMAP邮箱未配置，请在 config.json 中配置 email.qq_imap")
		return
	}

	server := cfg.Server
	if server == "" {
		server = "imap.qq.com"
	}
	port := cfg.Port
	if port == 0 {
		port = 993
	}

	log.Println("🔧 测试IMAP邮箱连接...")
	log.Printf("   服务器: %s:%d", server, port)
	log.Printf("   邮箱: %s", cfg.Address)

	// 连接IMAP服务器
	addr := fmt.Sprintf("%s:%d", server, port)
	log.Println("📡 正在连接IMAP服务器...")

	c, err := client.DialTLS(addr, &tls.Config{ServerName: server})
	if err != nil {
		log.Printf("❌ 连接IMAP服务器失败: %v", err)
		return
	}
	defer c.Logout()
	log.Println("✅ 连接成功")

	// 登录
	log.Println("🔐 正在登录...")
	if err := c.Login(cfg.Address, cfg.AuthCode); err != nil {
		log.Printf("❌ IMAP登录失败: %v", err)
		log.Println("   请检查邮箱地址和授权码是否正确")
		return
	}
	log.Println("✅ 登录成功")

	// 选择收件箱
	mbox, err := c.Select("INBOX", true)
	if err != nil {
		log.Printf("❌ 选择收件箱失败: %v", err)
		return
	}
	log.Printf("✅ 收件箱打开成功，共 %d 封邮件", mbox.Messages)

	if mbox.Messages == 0 {
		log.Println("📭 收件箱为空")
		return
	}

	// 获取最近5封邮件
	from := uint32(1)
	to := mbox.Messages
	if mbox.Messages > 5 {
		from = mbox.Messages - 4
	}

	log.Printf("📬 读取最近 %d 封邮件...", to-from+1)

	seqSet := new(imap.SeqSet)
	seqSet.AddRange(from, to)

	messages := make(chan *imap.Message, 10)
	section := &imap.BodySectionName{}
	items := []imap.FetchItem{section.FetchItem(), imap.FetchEnvelope}

	done := make(chan error, 1)
	go func() {
		done <- c.Fetch(seqSet, items, messages)
	}()

	count := 0
	for msg := range messages {
		count++
		if msg == nil || msg.Envelope == nil {
			log.Printf("   邮件 %d: (无法读取)", count)
			continue
		}

		subject := msg.Envelope.Subject
		date := msg.Envelope.Date.Format("2006-01-02 15:04:05")
		from := ""
		if len(msg.Envelope.From) > 0 && msg.Envelope.From[0] != nil {
			from = msg.Envelope.From[0].Address()
		}
		to := ""
		if len(msg.Envelope.To) > 0 && msg.Envelope.To[0] != nil {
			to = msg.Envelope.To[0].Address()
		}

		// 读取正文长度
		bodyLen := 0
		r := msg.GetBody(section)
		if r != nil {
			body, _ := io.ReadAll(r)
			bodyLen = len(body)
		}

		log.Printf("   邮件 %d:", count)
		log.Printf("      主题: %s", subject)
		log.Printf("      发件人: %s", from)
		log.Printf("      收件人: %s", to)
		log.Printf("      时间: %s", date)
		log.Printf("      正文长度: %d 字节", bodyLen)
	}

	if err := <-done; err != nil {
		log.Printf("❌ 获取邮件失败: %v", err)
		return
	}

	log.Println("✅ IMAP测试完成")
}

// getVerificationCodeFromQQMail 从IMAP邮箱获取验证码（支持Gmail/QQ/163等任何IMAP服务）
// targetEmail: 注册用的邮箱地址（用于匹配收件人）
// maxWait: 最大等待时间
func getVerificationCodeFromQQMail(targetEmail string, maxWait time.Duration) (string, error) {
	cfg := appConfig.Email.QQImap
	if cfg.Address == "" || cfg.AuthCode == "" {
		return "", fmt.Errorf("IMAP邮箱未配置")
	}

	server := cfg.Server
	if server == "" {
		server = "imap.qq.com"
	}
	port := cfg.Port
	if port == 0 {
		port = 993
	}

	// 使用 UTC 时间，因为 IMAP 邮件时间通常是 UTC
	startTime := time.Now().UTC()
	checkInterval := 1 * time.Second // 1秒检查一次，更快
	checkCount := 0

	// 提取目标邮箱的用户名部分（用于在邮件正文中搜索）
	targetUser := strings.Split(targetEmail, "@")[0]

	log.Printf("📬 开始从IMAP邮箱获取验证码，IMAP服务器: %s:%d，监听邮箱: %s，目标注册邮箱: %s (用户名: %s), 开始时间: %s UTC",
		server, port, cfg.Address, targetEmail, targetUser, startTime.Format("15:04:05"))

	for time.Since(startTime) < maxWait {
		checkCount++
		// 传入开始时间，只接受这个时间之后的邮件
		code, err := checkQQMailForCode(server, port, cfg.Address, cfg.AuthCode, targetEmail, startTime)
		if err != nil {
			log.Printf("⚠️ [检查 %d] IMAP邮箱检查失败: %v", checkCount, err)
		} else if code != "" {
			log.Printf("✅ 从IMAP邮箱获取到验证码: %s (服务器: %s:%d, 耗时 %v)", code, server, port, time.Since(startTime))
			return code, nil
		} else {
			// 安静模式：不再打印每轮检查日志
		}
		time.Sleep(checkInterval)
	}

	return "", fmt.Errorf("等待验证码超时 (%v)，请检查：1.IMAP邮箱(%s)是否收到Google邮件 2.邮件转发是否正常", maxWait, cfg.Address)
}

// checkQQMailForCode 检查IMAP邮箱中的验证码邮件
// startTime: 只接受这个时间之后收到的邮件
func checkQQMailForCode(server string, port int, email, authCode, targetEmail string, startTime time.Time) (string, error) {
	// 控制邮件调试日志量，true 时输出详细调试信息
	const verboseEmailLog = true

	// 连接IMAP服务器
	addr := fmt.Sprintf("%s:%d", server, port)
	c, err := client.DialTLS(addr, &tls.Config{ServerName: server})
	if err != nil {
		return "", fmt.Errorf("连接IMAP服务器失败: %w", err)
	}
	defer c.Logout()

	// 登录
	if err := c.Login(email, authCode); err != nil {
		return "", fmt.Errorf("IMAP登录失败: %w", err)
	}

	// 检查连接状态 - 发送 NOOP 命令刷新状态
	if err := c.Noop(); err != nil {
		return "", fmt.Errorf("IMAP 状态刷新失败: %w", err)
	}

	// 选择收件箱（只读模式）
	mbox, err := c.Select("INBOX", true)
	if err != nil {
		return "", fmt.Errorf("选择收件箱失败: %w", err)
	}

	if verboseEmailLog {
		log.Printf("📬 收件箱共 %d 封邮件 (最近: %d, 未读: %d)", mbox.Messages, mbox.Recent, mbox.Unseen)
	}

	if mbox.Messages == 0 {
		return "", nil // 没有邮件
	}

	// 搜索最近的邮件（最近20封）
	from := uint32(1)
	to := mbox.Messages
	if mbox.Messages > 20 {
		from = mbox.Messages - 19
	}

	if verboseEmailLog {
		log.Printf("📬 收件箱共 %d 封邮件，检查第 %d-%d 封", mbox.Messages, from, to)
	}

	seqSet := new(imap.SeqSet)
	seqSet.AddRange(from, to)

	// 获取邮件（包含完整头部信息）
	messages := make(chan *imap.Message, 20)
	section := &imap.BodySectionName{}
	headerSection := &imap.BodySectionName{Peek: true}
	headerSection.Specifier = imap.HeaderSpecifier

	items := []imap.FetchItem{
		section.FetchItem(),
		imap.FetchEnvelope,
		headerSection.FetchItem(), // 获取完整邮件头
	}

	done := make(chan error, 1)
	go func() {
		done <- c.Fetch(seqSet, items, messages)
	}()

	// 提取目标邮箱的用户名部分（用于在邮件正文中搜索）
	targetUser := strings.Split(targetEmail, "@")[0]
	checkedCount := 0
	fallbackCode := ""
	googleMailCount := 0

	// 检查每封邮件
	for msg := range messages {
		if msg == nil {
			continue
		}
		checkedCount++

		if msg.Envelope == nil {
			log.Printf("⚠️ 邮件 %d: Envelope 为空", checkedCount)
			continue
		}

		subject := msg.Envelope.Subject
		// 将邮件时间转换为 UTC，确保与 startTime 时区一致
		msgDate := msg.Envelope.Date.UTC()

		// 获取发件人
		fromAddr := ""
		if len(msg.Envelope.From) > 0 && msg.Envelope.From[0] != nil {
			fromAddr = msg.Envelope.From[0].Address()
		}

		// 获取收件人列表
		toAddrs := []string{}
		for _, addr := range msg.Envelope.To {
			if addr != nil {
				toAddrs = append(toAddrs, addr.Address())
			}
		}

		// 读取邮件头，查找原始收件人（转发邮件）
		headerSection := &imap.BodySectionName{Peek: true}
		headerSection.Specifier = imap.HeaderSpecifier
		headerReader := msg.GetBody(headerSection)
		originalRecipients := []string{}
		if headerReader != nil {
			headerBytes, _ := io.ReadAll(headerReader)
			headerStr := string(headerBytes)

			// 查找可能包含原始收件人的字段
			for _, line := range strings.Split(headerStr, "\n") {
				line = strings.TrimSpace(line)
				// X-Forwarded-To, Delivered-To, X-Original-To 等
				if strings.HasPrefix(line, "X-Forwarded-To:") ||
					strings.HasPrefix(line, "Delivered-To:") ||
					strings.HasPrefix(line, "X-Original-To:") {
					addr := strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
					originalRecipients = append(originalRecipients, addr)
				}
			}
		}

		// 先打印所有邮件信息用于调试
		if verboseEmailLog {
			log.Printf("🔍 邮件 %d: 主题='%s', 发件人='%s', 时间=%v UTC",
				checkedCount, subject, fromAddr, msgDate.Format("15:04:05"))
			log.Printf("   收件人: %v, 原始收件人: %v", toAddrs, originalRecipients)
		}

		// 关键修改：只处理在 startTime 之后收到的邮件（允许30秒误差）
		// 这样可以避免读取旧的验证码邮件
		if msgDate.Before(startTime.Add(-30 * time.Second)) {
			if verboseEmailLog {
				log.Printf("   ⏭️ 跳过：邮件时间 %v 早于开始时间 %v",
					msgDate.Format("15:04:05"), startTime.Format("15:04:05"))
			}
			continue
		}

		// 读取邮件正文
		r := msg.GetBody(section)
		if r == nil {
			log.Printf("⚠️ 邮件 %d: 无法获取正文, 主题=%s", checkedCount, subject)
			continue
		}

		body, err := io.ReadAll(r)
		if err != nil {
			log.Printf("⚠️ 邮件 %d: 读取正文失败: %v", checkedCount, err)
			continue
		}
		bodyStr := string(body)

		// 检查是否是Google的验证邮件（放宽条件）
		isGoogleMail := strings.Contains(subject, "验证") || strings.Contains(subject, "Verify") ||
			strings.Contains(subject, "code") || strings.Contains(subject, "Code") ||
			strings.Contains(subject, "Google") || strings.Contains(subject, "google") ||
			strings.Contains(bodyStr, "Google") || strings.Contains(bodyStr, "验证码") ||
			strings.Contains(fromAddr, "google")

		if !isGoogleMail {
			continue
		}

		googleMailCount++
		if verboseEmailLog {
			log.Printf("📧 [Google邮件 %d] 主题: %s, 发件人: %s, 时间: %v",
				googleMailCount, subject, fromAddr, msgDate.Format("15:04:05"))
		}

		// 检查邮件是否与目标邮箱相关
		toMatched := false
		// 检查常规收件人
		for _, addr := range toAddrs {
			if strings.EqualFold(addr, targetEmail) {
				toMatched = true
				break
			}
		}
		// 检查原始收件人（转发邮件）
		originalMatched := false
		for _, addr := range originalRecipients {
			if strings.Contains(addr, targetEmail) || strings.Contains(addr, targetUser) {
				originalMatched = true
				break
			}
		}

		// 检查正文是否包含目标邮箱地址或用户名
		bodyContainsTarget := strings.Contains(bodyStr, targetEmail) || strings.Contains(bodyStr, targetUser)

		// 匹配条件：收件人匹配 或 原始收件人匹配，正文命中作为兜底
		if verboseEmailLog {
			log.Printf("   收件人匹配=%v, 原始收件人匹配=%v, 正文包含目标=%v",
				toMatched, originalMatched, bodyContainsTarget)
		}

		targetMatched := toMatched || originalMatched
		if !targetMatched && !bodyContainsTarget {
			continue
		}

		// 从邮件内容中提取验证码
		code, err := extractVerificationCode(bodyStr)
		if verboseEmailLog {
			log.Printf("   🔍 验证码提取结果: code='%s', err=%v", code, err)
		}
		if err == nil && code != "" {
			if targetMatched {
				log.Printf("✅ 从邮件正文提取到验证码: %s (收件人命中)", code)
				return code, nil
			}
			// 正文兜底先记录，继续找有没有收件人命中的更优邮件
			if fallbackCode == "" {
				fallbackCode = code
				log.Printf("✅ 从正文兜底提取验证码（收件人未命中）: %s", code)
			}
		} else if verboseEmailLog {
			log.Printf("   ⚠️ 未能从正文提取验证码")
		}

		// 也尝试从主题中提取
		code, err = extractVerificationCode(subject)
		if err == nil && code != "" {
			if targetMatched {
				log.Printf("✅ 从邮件主题提取到验证码: %s (收件人命中)", code)
				return code, nil
			}
			if fallbackCode == "" {
				fallbackCode = code
				log.Printf("✅ 从主题兜底提取验证码（收件人未命中）: %s", code)
			}
		}

		// 打印正文前500字符用于调试
		preview := bodyStr
		if len(preview) > 500 {
			preview = preview[:500]
		}
		if verboseEmailLog {
			log.Printf("   📄 邮件正文预览(前500字符):\n%s\n   ---", preview)

			// 解码后的内容
			decoded := decodeMimeContent(bodyStr)
			decodedPreview := decoded
			if len(decodedPreview) > 500 {
				decodedPreview = decodedPreview[:500]
			}
			log.Printf("   📝 解码后内容预览(前500字符):\n%s\n   ---", decodedPreview)
		}
	}

	// 检查 fetch 是否有错误
	if err := <-done; err != nil {
		return "", fmt.Errorf("获取邮件失败: %w", err)
	}

	// 没有收件人命中的邮件，但有兜底验证码
	if fallbackCode != "" {
		return fallbackCode, nil
	}

	if verboseEmailLog {
		log.Printf("📊 共检查 %d 封邮件，其中 %d 封是Google邮件", checkedCount, googleMailCount)
	}
	return "", nil // 未找到验证码
}

// getEmailCount 获取当前邮件数量
func getEmailCount(email string) int {
	// 如果使用IMAP邮箱，不需要计数
	if isQQImapConfigured() {
		return 0
	}

	req, _ := http.NewRequest("GET", fmt.Sprintf("https://mail.chatgpt.org.uk/api/emails?email=%s", email), nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36")
	req.Header.Set("Referer", "https://mail.chatgpt.org.uk")

	resp, err := httpClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()

	body, _ := readResponseBody(resp)
	var result EmailListResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return 0
	}
	return len(result.Data.Emails)
}

// getVerificationCode 统一的验证码获取函数
// 优先使用IMAP邮箱（Gmail/QQ/163等），回退到临时邮箱API
func getVerificationCode(targetEmail string, maxWait time.Duration) (string, error) {
	// 优先使用IMAP邮箱
	if isQQImapConfigured() {
		return getVerificationCodeFromQQMail(targetEmail, maxWait)
	}

	// 回退到临时邮箱API
	retries := int(maxWait.Seconds() / 3)
	if retries < 1 {
		retries = 1
	}
	emailContent, err := getVerificationEmailQuick(targetEmail, retries, 3)
	if err != nil {
		return "", err
	}
	return extractVerificationCode(emailContent.Content)
}

func getVerificationEmailQuick(email string, retries int, intervalSec int) (*EmailContent, error) {
	return getVerificationEmailAfter(email, retries, intervalSec, 0)
}

// getVerificationEmailAfter 获取包含有效验证码的新邮件
func getVerificationEmailAfter(email string, retries int, intervalSec int, initialCount int) (*EmailContent, error) {
	for i := 0; i < retries; i++ {
		req, _ := http.NewRequest("GET", fmt.Sprintf("https://mail.chatgpt.org.uk/api/emails?email=%s", email), nil)
		req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36")
		req.Header.Set("Referer", "https://mail.chatgpt.org.uk")

		resp, err := httpClient.Do(req)
		if err != nil {
			time.Sleep(time.Duration(intervalSec) * time.Second)
			continue
		}

		body, _ := readResponseBody(resp)
		resp.Body.Close()

		var result EmailListResponse
		if err := json.Unmarshal(body, &result); err != nil {
			time.Sleep(time.Duration(intervalSec) * time.Second)
			continue
		}

		// 检查是否有新邮件（数量增加）且包含有效验证码
		if result.Success && len(result.Data.Emails) > initialCount {
			// 验证最新邮件是否包含有效验证码
			latestEmail := &result.Data.Emails[0]
			if _, err := extractVerificationCode(latestEmail.Content); err == nil {
				return latestEmail, nil
			}
			// 验证码提取失败，继续等待新邮件
		}
		time.Sleep(time.Duration(intervalSec) * time.Second)
	}
	return nil, fmt.Errorf("未收到验证码邮件")
}

func extractVerificationCode(content string) (string, error) {
	log.Printf("🔍 [验证码提取] 开始提取验证码，内容长度: %d 字节", len(content))

	// 先尝试解析 MIME 内容
	decodedContent := decodeMimeContent(content)
	if len(decodedContent) != len(content) {
		log.Printf("   📝 MIME解码后长度: %d 字节", len(decodedContent))
	}

	// 0) 关键词附近优先提取（常见“验证码/verification code/one-time code”）
	// 仅允许关键词后最多40个非数字字符，取到第一段即返回，避免抓到正文其它ID
	log.Printf("   🔍 策略1: 关键词附近提取...")
	reKeyword := regexp.MustCompile(`(?i)(?:验证码|verification code|one[-\\s]?time code|one[-\\s]?time password|otp|code)\\D{0,40}([A-Z0-9]{6})`)
	if m := reKeyword.FindStringSubmatch(decodedContent); len(m) > 1 {
		log.Printf("   ✅ 通过关键词匹配找到验证码: %s", m[1])
		return m[1], nil
	}

	// Google 验证码格式通常是: G-XXXXXX 或纯6位字母数字
	// 优先匹配 G- 开头的格式
	log.Printf("   🔍 策略2: Google格式 (G-XXXXXX)...")
	reGoogle := regexp.MustCompile(`G-([A-Z0-9]{6})`)
	if m := reGoogle.FindStringSubmatch(decodedContent); len(m) > 1 {
		log.Printf("   ✅ 通过Google格式找到验证码: %s", m[1])
		return m[1], nil
	}

	// 匹配6位大写字母数字组合
	log.Printf("   🔍 策略3: 通用6位字符匹配...")
	re := regexp.MustCompile(`\b([A-Z0-9]{6})\b`)
	matches := re.FindAllStringSubmatch(decodedContent, -1)
	log.Printf("   📊 找到 %d 个6位字符候选", len(matches))

	hasLetterRe := regexp.MustCompile(`[A-Z]`)
	hasDigitRe := regexp.MustCompile(`[0-9]`)
	pureLetterRe := regexp.MustCompile(`^[A-Z]{6}$`)
	for i, match := range matches {
		code := match[1]
		log.Printf("   🔎 候选 %d: %s", i+1, code)
		if commonWords[code] {
			log.Printf("      ⏭️ 跳过常见词: %s", code)
			continue
		}
		hasLetter := hasLetterRe.MatchString(code)
		hasDigit := hasDigitRe.MatchString(code)
		// 先取字母数字混合（最常见也最可靠）
		if hasLetter && hasDigit {
			log.Printf("   ✅ 找到字母数字混合验证码: %s", code)
			return code, nil
		}
		// 再取纯字母（已过滤常见无效词/全相同）
		if hasLetter && !hasDigit && pureLetterRe.MatchString(code) {
			if isAllSameChar(code) {
				continue
			}
			switch code {
			case "REJECT", "VERIFY", "CANCEL", "GOOGLE":
				continue
			}
			return code, nil
		}
	}

	// 如果没有找到字母数字混合的，尝试只有数字的（纯字母容易误判为 REJECT 等）
	for _, match := range matches {
		code := match[1]
		if commonWords[code] {
			continue
		}
		// 仅接受纯数字，避免 REJECT 这类全字母串
		if !regexp.MustCompile(`^[0-9]{6}$`).MatchString(code) {
			continue
		}
		// 排除全是相同数字的情况（如 333333, 000000）
		if isAllSameChar(code) {
			continue
		}
		// 排除看起来像日期/时间的（如 202312, 143052）
		if looksLikeDateTime(code) {
			continue
		}
		return code, nil
	}

	// 最后尝试从 "code is" 或 "验证码" 附近提取
	log.Printf("   🔍 策略4: \"code is\" 模式...")
	re2 := regexp.MustCompile(`(?i)(?:code|验证码)\s*[:is：]\s*([A-Z0-9]{6})`)
	if m := re2.FindStringSubmatch(decodedContent); len(m) > 1 {
		log.Printf("   ✅ 通过 \"code is\" 模式找到验证码: %s", m[1])
		return m[1], nil
	}

	log.Printf("   ❌ 所有策略均未找到验证码")
	return "", fmt.Errorf("无法从邮件中提取验证码")
}

// decodeMimeContent 解码 MIME 邮件内容
func decodeMimeContent(content string) string {
	result := content

	// 处理 multipart 邮件，提取所有部分
	if strings.Contains(strings.ToLower(content), "content-type: multipart") {
		parts := strings.Split(content, "\n")
		var extracted strings.Builder

		for i := 0; i < len(parts); i++ {
			line := parts[i]

			// 检测到 Content-Transfer-Encoding
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "content-transfer-encoding:") {
				encoding := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.ToLower(line), "content-transfer-encoding:")))

				// 跳过头部，找到实际内容
				i++
				for i < len(parts) && strings.TrimSpace(parts[i]) != "" {
					i++
				}
				if i >= len(parts) {
					break
				}
				i++ // 跳过空行

				// 收集内容直到下一个边界或结尾
				var contentBuilder strings.Builder
				for i < len(parts) {
					if strings.HasPrefix(parts[i], "--") ||
						strings.HasPrefix(strings.ToLower(strings.TrimSpace(parts[i])), "content-") {
						break
					}
					contentBuilder.WriteString(parts[i] + "\n")
					i++
				}

				partContent := contentBuilder.String()

				// 根据编码解码
				if strings.Contains(encoding, "base64") {
					// 清理内容，移除空格和换行
					cleaned := strings.ReplaceAll(partContent, "\n", "")
					cleaned = strings.ReplaceAll(cleaned, "\r", "")
					cleaned = strings.TrimSpace(cleaned)
					if decoded, err := base64.StdEncoding.DecodeString(cleaned); err == nil {
						extracted.WriteString(string(decoded) + "\n")
					}
				} else if strings.Contains(encoding, "quoted-printable") {
					reader := quotedprintable.NewReader(strings.NewReader(partContent))
					if decoded, err := io.ReadAll(reader); err == nil {
						extracted.WriteString(string(decoded) + "\n")
					}
				} else {
					extracted.WriteString(partContent + "\n")
				}
				i--
			}
		}

		if extracted.Len() > 0 {
			result = extracted.String()
		}
	}

	// 尝试解码 Base64 内容（单部分邮件）
	if strings.Contains(content, "Content-Transfer-Encoding: base64") ||
		strings.Contains(content, "content-transfer-encoding: base64") {
		// 查找 Base64 编码的部分
		lines := strings.Split(content, "\n")
		var base64Content strings.Builder
		inBase64 := false
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" && inBase64 {
				continue
			}
			if strings.HasPrefix(line, "Content-") || strings.HasPrefix(line, "content-") {
				if strings.Contains(strings.ToLower(line), "base64") {
					inBase64 = true
				}
				continue
			}
			if inBase64 && line != "" && !strings.Contains(line, ":") && !strings.HasPrefix(line, "--") {
				base64Content.WriteString(line)
			}
		}
		if base64Content.Len() > 0 {
			if decoded, err := base64.StdEncoding.DecodeString(base64Content.String()); err == nil {
				result = string(decoded)
			}
		}
	}

	// 尝试解码 Quoted-Printable 内容（单部分邮件）
	if strings.Contains(content, "Content-Transfer-Encoding: quoted-printable") ||
		strings.Contains(content, "content-transfer-encoding: quoted-printable") {
		// 查找并解码 QP 内容
		reader := quotedprintable.NewReader(strings.NewReader(content))
		if decoded, err := io.ReadAll(reader); err == nil && len(decoded) > 0 {
			result = string(decoded)
		}
	}

	// 解码 MIME 编码的主题/内容 (=?UTF-8?B?...?= 或 =?UTF-8?Q?...?=)
	dec := new(mime.WordDecoder)
	if decoded, err := dec.DecodeHeader(result); err == nil {
		result = decoded
	}

	// 移除 HTML 标签，提取纯文本
	result = stripHTMLTags(result)

	return result
}

// stripHTMLTags 移除 HTML 标签
func stripHTMLTags(s string) string {
	re := regexp.MustCompile(`<[^>]*>`)
	return re.ReplaceAllString(s, " ")
}

// isAllSameChar 检查是否全是相同字符
func isAllSameChar(s string) bool {
	if len(s) == 0 {
		return true
	}
	first := s[0]
	for i := 1; i < len(s); i++ {
		if s[i] != first {
			return false
		}
	}
	return true
}

// looksLikeDateTime 检查是否看起来像日期时间
func looksLikeDateTime(s string) bool {
	// 检查是否像年月日 (202312) 或时分秒 (143052)
	if len(s) != 6 {
		return false
	}
	// 检查前4位是否像年份 (2020-2030)
	if s[:4] >= "2020" && s[:4] <= "2030" {
		return true
	}
	// 检查是否像时间格式
	hour := s[:2]
	min := s[2:4]
	sec := s[4:6]
	if hour >= "00" && hour <= "23" && min >= "00" && min <= "59" && sec >= "00" && sec <= "59" {
		// 可能是时间，但不一定
		return false // 不排除，因为验证码也可能是这种格式
	}
	return false
}
func safeType(page *rod.Page, text string, delay int) error {
	for _, char := range text {
		if err := page.Keyboard.Type(input.Key(char)); err != nil {
			return err
		}
		time.Sleep(time.Duration(delay) * time.Millisecond)
	}
	return nil
}

// debugScreenshot 调试截图
func debugScreenshot(page *rod.Page, threadID int, step string) {
	if !RegisterDebug {
		return
	}
	screenshotDir := filepath.Join(DataDir, "screenshots")
	os.MkdirAll(screenshotDir, 0755)

	filename := filepath.Join(screenshotDir, fmt.Sprintf("thread%d_%s_%d.png", threadID, step, time.Now().Unix()))
	data, err := page.Screenshot(true, nil)
	if err != nil {
		log.Printf("[注册 %d] 📸 截图失败: %v", threadID, err)
		return
	}
	if err := os.WriteFile(filename, data, 0644); err != nil {
		log.Printf("[注册 %d] 📸 保存截图失败: %v", threadID, err)
		return
	}
	log.Printf("[注册 %d] 📸 截图保存: %s", threadID, filename)
}

// handleAdditionalSteps 处理额外步骤（复选框等）
func handleAdditionalSteps(page *rod.Page, threadID int) bool {
	log.Printf("[注册 %d] 检查是否需要处理额外步骤...", threadID)

	hasAdditionalSteps := false

	// 检查是否需要同意条款（主要处理复选框）
	checkboxResult, _ := page.Eval(`() => {
		const checkboxes = document.querySelectorAll('input[type="checkbox"]');
		for (const checkbox of checkboxes) {
			if (!checkbox.checked) {
				checkbox.click();
				return { clicked: true };
			}
		}
		return { clicked: false };
	}`)

	if checkboxResult != nil && checkboxResult.Value.Get("clicked").Bool() {
		hasAdditionalSteps = true
		log.Printf("[注册 %d] 已勾选条款复选框", threadID)
		time.Sleep(1 * time.Second)
	}

	// 如果有额外步骤，尝试提交
	if hasAdditionalSteps {
		log.Printf("[注册 %d] 发现有额外步骤，尝试提交...", threadID)

		// 尝试提交额外信息
		for i := 0; i < 3; i++ {
			submitResult, _ := page.Eval(`() => {
				const submitButtons = [
					...document.querySelectorAll('button'),
					...document.querySelectorAll('input[type="submit"]')
				];
				
				for (const button of submitButtons) {
					if (!button.disabled && button.offsetParent !== null) {
						const text = button.textContent || '';
						if (text.includes('同意') || text.includes('Confirm') || 
							text.includes('继续') || text.includes('Next') || 
							text.includes('Submit') || text.includes('完成')) {
							button.click();
							return { clicked: true };
						}
					}
				}
				
				// 点击第一个可用的提交按钮
				for (const button of submitButtons) {
					if (!button.disabled && button.offsetParent !== null) {
						button.click();
						return { clicked: true };
					}
				}
				
				return { clicked: false };
			}`)

			if submitResult != nil && submitResult.Value.Get("clicked").Bool() {
				log.Printf("[注册 %d] 已提交额外信息", threadID)
				break
			}

			time.Sleep(1 * time.Second)
		}

		// 等待可能的跳转
		time.Sleep(3 * time.Second)
		return true
	}

	return false
}

// checkAndHandleAdminPage 检查并处理管理创建页面
func checkAndHandleAdminPage(page *rod.Page, threadID int) bool {
	currentURL := ""
	info, _ := page.Info()
	if info != nil {
		currentURL = info.URL
	}

	// 检查是否是管理创建页面
	if strings.Contains(currentURL, "/admin/create") {
		log.Printf("[注册 %d] 检测到管理创建页面，尝试完成设置...", threadID)

		// 尝试查找并点击继续按钮
		formCompleted, _ := page.Eval(`() => {
			let completed = false;
			
			// 查找并点击继续按钮
			const continueTexts = ['Continue', '继续', 'Next', 'Submit', 'Finish', '完成'];
			const allButtons = document.querySelectorAll('button');
			
			for (const button of allButtons) {
				if (button.offsetParent !== null && !button.disabled) {
					const text = (button.textContent || '').trim();
					if (continueTexts.some(t => text.includes(t))) {
						button.click();
						console.log('点击继续按钮:', text);
						completed = true;
						return completed;
					}
				}
			}
			
			// 如果没有找到特定按钮，尝试点击第一个可见按钮
			for (const button of allButtons) {
				if (button.offsetParent !== null && !button.disabled) {
					const text = button.textContent || '';
					if (text.trim() && !text.includes('Cancel') && !text.includes('取消')) {
						button.click();
						console.log('点击通用按钮:', text);
						completed = true;
						break;
					}
				}
			}
			
			return completed;
		}`)

		if formCompleted != nil && formCompleted.Value.Bool() {
			log.Printf("[注册 %d] 已处理管理表单，等待跳转...", threadID)
			time.Sleep(5 * time.Second)
			return true
		}
	}

	return false
}

func RunBrowserRegister(headless bool, proxy string, threadID int) (result *BrowserRegisterResult) {
	log.Printf("🎬 [注册 %d] ========== 开始注册流程 ==========", threadID)
	log.Printf("📋 [注册 %d] 配置: headless=%v, proxy=%s", threadID, headless, proxy)

	result = &BrowserRegisterResult{}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[注册 %d] ☠️ panic 恢复: %v", threadID, r)
			result.Error = fmt.Errorf("panic: %v", r)
		}
	}()

	// 获取临时邮箱
	log.Printf("📧 [注册 %d] 步骤 1/8: 获取临时邮箱...", threadID)
	email, err := getTemporaryEmail()
	if err != nil {
		log.Printf("❌ [注册 %d] 获取临时邮箱失败: %v", threadID, err)
		result.Error = err
		return result
	}
	result.Email = email
	log.Printf("✅ [注册 %d] 获取到邮箱: %s", threadID, email)

	// 启动浏览器 - 优先使用系统浏览器
	log.Printf("🌐 [注册 %d] 步骤 2/8: 启动浏览器...", threadID)
	l := launcher.New()

	// 检测系统浏览器（支持更多环境）
	log.Printf("🔍 [注册 %d] 检测系统浏览器...", threadID)
	systemBrowsers := []string{
		// Linux
		"/usr/bin/google-chrome",
		"/usr/bin/google-chrome-stable",
		"/usr/bin/chromium",
		"/usr/bin/chromium-browser",
		"/snap/bin/chromium",
		"/opt/google/chrome/chrome",
		// Docker/Alpine
		"/usr/bin/chromium-browser",
		"/usr/lib/chromium/chromium",
		// Windows
		"C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe",
		"C:\\Program Files (x86)\\Google\\Chrome\\Application\\chrome.exe",
		// macOS
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
	}

	browserFound := false
	for _, path := range systemBrowsers {
		if _, err := os.Stat(path); err == nil {
			l = l.Bin(path)
			browserFound = true
			log.Printf("✅ [注册 %d] 使用浏览器: %s", threadID, path)
			break
		}
	}

	if !browserFound {
		log.Printf("⚠️ [注册 %d] 未找到系统浏览器，尝试使用 rod 自动下载", threadID)
	}

	// 设置启动参数（兼容更多环境 + 增强反检测）
	log.Printf("⚙️ [注册 %d] 配置浏览器启动参数 (headless=%v)...", threadID, headless)
	l = l.Headless(headless).
		Set("no-sandbox").
		Set("disable-setuid-sandbox").
		Set("disable-dev-shm-usage").
		Set("disable-gpu").
		Set("disable-software-rasterizer").
		Set("disable-blink-features", "AutomationControlled").
		Set("window-size", "1280,800").
		Set("lang", "zh-CN").
		Set("disable-extensions").
		Set("exclude-switches", "enable-automation").
		Set("disable-infobars").
		Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")

	if proxy != "" {
		log.Printf("🔀 [注册 %d] 使用代理: %s", threadID, proxy)
		l = l.Proxy(proxy)
	}

	log.Printf("🚀 [注册 %d] 启动浏览器实例...", threadID)
	url, err := l.Launch()
	if err != nil {
		result.Error = fmt.Errorf("启动浏览器失败: %w", err)
		return result
	}

	browser := rod.New().ControlURL(url)
	if err := browser.Connect(); err != nil {
		result.Error = fmt.Errorf("连接浏览器失败: %w", err)
		return result
	}
	defer browser.Close()

	browser = browser.Timeout(120 * time.Second)

	// 获取默认页面
	pages, _ := browser.Pages()
	var page *rod.Page
	if len(pages) > 0 {
		page = pages[0]
	} else {
		page, _ = browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
	}

	// 设置视口和 User-Agent
	page.MustSetViewport(1280, 800, 1, false)
	page.SetUserAgent(&proto.NetworkSetUserAgentOverride{
		UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	})

	// 增强的反检测脚本
	page.Eval(`() => {
		// 删除 webdriver 标识
		Object.defineProperty(navigator, 'webdriver', {
			get: () => undefined
		});
		
		// 修复 Chrome 对象
		window.chrome = {
			runtime: {},
			loadTimes: function() {},
			csi: function() {},
			app: {}
		};
		
		// 修复 Permissions API
		const originalQuery = window.navigator.permissions.query;
		window.navigator.permissions.query = (parameters) => (
			parameters.name === 'notifications' ?
				Promise.resolve({ state: Notification.permission }) :
				originalQuery(parameters)
		);
		
		// 修复 plugins
		Object.defineProperty(navigator, 'plugins', {
			get: () => [1, 2, 3, 4, 5]
		});
		
		// 修复 languages
		Object.defineProperty(navigator, 'languages', {
			get: () => ['zh-CN', 'zh', 'en']
		});
	}`)

	// 监听请求以捕获 authorization
	var authorization string
	var configID, csesidx string

	go page.EachEvent(func(e *proto.NetworkRequestWillBeSent) {
		if auth, ok := e.Request.Headers["authorization"]; ok {
			if authStr := auth.String(); authStr != "" {
				authorization = authStr
			}
		}
		url := e.Request.URL
		if m := regexp.MustCompile(`/cid/([a-f0-9-]+)`).FindStringSubmatch(url); len(m) > 1 && configID == "" {
			configID = m[1]
		}
		if m := regexp.MustCompile(`[?&]csesidx=(\d+)`).FindStringSubmatch(url); len(m) > 1 && csesidx == "" {
			csesidx = m[1]
		}
	})()
	log.Printf("🌍 [注册 %d] 步骤 3/8: 打开注册页面...", threadID)
	if err := page.Navigate("https://business.gemini.google"); err != nil {
		log.Printf("❌ [注册 %d] 打开页面失败: %v", threadID, err)
		result.Error = fmt.Errorf("打开页面失败: %w", err)
		return result
	}
	page.WaitLoad()
	log.Printf("✅ [注册 %d] 页面加载完成", threadID)
	time.Sleep(500 * time.Millisecond)
	debugScreenshot(page, threadID, "01_page_loaded")

	log.Printf("⏳ [注册 %d] 等待输入框出现（最多20秒）...", threadID)
	if _, err := page.Timeout(20 * time.Second).Element("input"); err != nil {
		log.Printf("❌ [注册 %d] 等待输入框超时: %v", threadID, err)
		result.Error = fmt.Errorf("等待输入框超时: %w", err)
		return result
	}
	log.Printf("✅ [注册 %d] 输入框已出现", threadID)
	time.Sleep(300 * time.Millisecond)

	// 点击输入框聚焦
	log.Printf("✍️ [注册 %d] 步骤 4/8: 输入邮箱地址...", threadID)
	log.Printf("📝 [注册 %d] 邮箱: %s", threadID, email)
	page.Eval(`() => {
		const inputs = document.querySelectorAll('input');
		if (inputs.length > 0) {
			inputs[0].click();
			inputs[0].focus();
		}
	}`)
	time.Sleep(200 * time.Millisecond)
	safeType(page, email, 15)
	time.Sleep(500 * time.Millisecond)

	// 触发 blur
	page.Eval(`() => {
		const inputs = document.querySelectorAll('input');
		if (inputs.length > 0) {
			inputs[0].blur();
		}
	}`)
	time.Sleep(500 * time.Millisecond)

	// 提前声明变量（避免 goto 跳过声明）
	var emailSubmitted bool
	var alreadyOnVerificationPage *proto.RuntimeRemoteObject

	// 策略1: 先等待3秒，检查是否自动跳转
	time.Sleep(3 * time.Second)

	// 检查是否已经跳转到验证码页面（更精确的判断）
	alreadyOnVerificationPage, _ = page.Eval(`() => {
		// 检查是否有验证码输入框（更可靠的判断）
		const inputs = document.querySelectorAll('input');
		let hasCodeInput = false;
		for (const input of inputs) {
			const placeholder = (input.placeholder || '').toLowerCase();
			const ariaLabel = (input.getAttribute('aria-label') || '').toLowerCase();
			if (placeholder.includes('code') || placeholder.includes('验证码') || 
			    ariaLabel.includes('code') || ariaLabel.includes('verification')) {
				hasCodeInput = true;
				break;
			}
		}
		
		// 检查页面文本（更严格的条件）
		const pageText = document.body ? document.body.textContent : '';
		const hasVerifyText = pageText.includes('验证码') || 
		                      pageText.includes('verification code') ||
		                      pageText.includes('Enter the code') ||
		                      pageText.includes('输入验证码');
		const hasNameText = pageText.includes('姓氏') || pageText.includes('名字') || 
		                    pageText.includes('Full name') || pageText.includes('全名') ||
		                    pageText.includes('First name') || pageText.includes('Last name');
		
		return {
			hasCodeInput: hasCodeInput,
			hasVerifyText: hasVerifyText,
			hasNameText: hasNameText,
			isVerificationPage: hasCodeInput || hasVerifyText,
			isNamePage: hasNameText,
			pageTextPreview: pageText.substring(0, 200)
		};
	}`)

	if alreadyOnVerificationPage != nil {
		isVerificationPage := alreadyOnVerificationPage.Value.Get("isVerificationPage").Bool()
		isNamePage := alreadyOnVerificationPage.Value.Get("isNamePage").Bool()

		if isVerificationPage || isNamePage {
			log.Printf("✅ [注册 %d] 邮箱提交成功，进入下一步", threadID)
			goto afterEmailSubmit
		}
	}

	// 策略2: 按 Enter 键提交
	page.Keyboard.Press(input.Enter)
	time.Sleep(3 * time.Second)

	// 再次检查是否跳转（使用同样的精确判断）
	alreadyOnVerificationPage, _ = page.Eval(`() => {
		const inputs = document.querySelectorAll('input');
		let hasCodeInput = false;
		for (const input of inputs) {
			const placeholder = (input.placeholder || '').toLowerCase();
			const ariaLabel = (input.getAttribute('aria-label') || '').toLowerCase();
			if (placeholder.includes('code') || placeholder.includes('验证码') || 
			    ariaLabel.includes('code') || ariaLabel.includes('verification')) {
				hasCodeInput = true;
				break;
			}
		}
		
		const pageText = document.body ? document.body.textContent : '';
		const hasVerifyText = pageText.includes('验证码') || 
		                      pageText.includes('verification code') ||
		                      pageText.includes('Enter the code');
		const hasNameText = pageText.includes('姓氏') || pageText.includes('Full name') || pageText.includes('全名');
		
		return {
			isVerificationPage: hasCodeInput || hasVerifyText,
			isNamePage: hasNameText,
			hasCodeInput: hasCodeInput,
			pageTextPreview: pageText.substring(0, 200)
		};
	}`)

	if alreadyOnVerificationPage != nil {
		isVerificationPage := alreadyOnVerificationPage.Value.Get("isVerificationPage").Bool()
		isNamePage := alreadyOnVerificationPage.Value.Get("isNamePage").Bool()

		if isVerificationPage || isNamePage {
			log.Printf("✅ [注册 %d] 邮箱提交成功，进入下一步", threadID)
			goto afterEmailSubmit
		}
	}

	// 策略3: 尝试查找并点击按钮（兜底）
	emailSubmitted = false
	for i := 0; i < 5; i++ {
		clickResult, _ := page.Eval(`() => {
			if (!document.body) return { clicked: false, reason: 'body_null' };
			
			const targets = ['继续', 'Next', '邮箱', 'Continue', 'Submit'];
			const elements = [
				...document.querySelectorAll('button'),
				...document.querySelectorAll('input[type="submit"]'),
				...document.querySelectorAll('div[role="button"]'),
				...document.querySelectorAll('span[role="button"]')
			];

			// 记录所有可见按钮用于调试
			let visibleButtons = [];
			for (const element of elements) {
				if (!element) continue;
				const style = window.getComputedStyle(element);
				if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') continue;
				if (element.disabled) continue;
				
				const text = element.textContent ? element.textContent.trim() : '';
				visibleButtons.push(text);
				
				if (targets.some(t => text.includes(t))) {
					element.click();
					return { clicked: true, text: text, allButtons: visibleButtons };
				}
			}
			return { clicked: false, reason: 'no_button', allButtons: visibleButtons };
		}`)

		if clickResult != nil {
			clicked := clickResult.Value.Get("clicked").Bool()

			if clicked {
				buttonText := clickResult.Value.Get("text").String()
				emailSubmitted = true
				log.Printf("✅ [注册 %d] 找到并点击提交按钮: '%s'", threadID, buttonText)
				time.Sleep(3 * time.Second)
				break
			}
		}
		time.Sleep(1 * time.Second)
	}

	// 策略4: 即使没找到按钮，也检查页面状态，不要立即报错
	if !emailSubmitted {
		time.Sleep(2 * time.Second)

		// 获取当前页面URL和详细状态
		info, _ := page.Info()
		currentURL := ""
		if info != nil {
			currentURL = info.URL
		}

		// 最后检查是否在正确页面（使用精确判断）
		alreadyOnVerificationPage, _ = page.Eval(`() => {
			const inputs = document.querySelectorAll('input');
			let hasCodeInput = false;
			let inputDetails = [];
			for (const input of inputs) {
				const placeholder = input.placeholder || '';
				const type = input.type || '';
				const ariaLabel = input.getAttribute('aria-label') || '';
				inputDetails.push({ type, placeholder, ariaLabel });
				
				if (placeholder.toLowerCase().includes('code') || 
				    placeholder.includes('验证码') || 
				    ariaLabel.toLowerCase().includes('code') ||
				    ariaLabel.toLowerCase().includes('verification')) {
					hasCodeInput = true;
				}
			}
			
			const pageText = document.body ? document.body.textContent : '';
			const hasVerifyText = pageText.includes('验证码') || 
			                      pageText.includes('verification code') ||
			                      pageText.includes('Enter the code');
			const hasNameText = pageText.includes('姓氏') || pageText.includes('Full name') || pageText.includes('全名');
			
			return {
				isVerificationPage: hasCodeInput || hasVerifyText,
				isNamePage: hasNameText,
				hasCodeInput: hasCodeInput,
				inputDetails: inputDetails,
				pageTextPreview: pageText.substring(0, 300)
			};
		}`)

		if alreadyOnVerificationPage != nil {
			isVerificationPage := alreadyOnVerificationPage.Value.Get("isVerificationPage").Bool()
			isNamePage := alreadyOnVerificationPage.Value.Get("isNamePage").Bool()

			if !isVerificationPage && !isNamePage {
				debugScreenshot(page, threadID, "error_no_submit")
				result.Error = fmt.Errorf("无法提交邮箱：页面未跳转且找不到提交按钮。当前URL: %s", currentURL)
				return result
			}
			log.Printf("✅ [注册 %d] 邮箱提交成功，进入下一步", threadID)
		}
	}

afterEmailSubmit:
	time.Sleep(2 * time.Second)

	// 获取当前URL确认状态
	info, _ := page.Info()
	if info != nil {
		log.Printf("🌐 [注册 %d] 提交后URL: %s", threadID, info.URL)
	}

	var needsVerification bool
	checkResult, _ := page.Eval(`() => {
		const pageText = document.body ? document.body.textContent : '';
		
		// 先检查是否是验证码页面（正常流程）
		const isVerificationPage = pageText.includes('验证码') || pageText.includes('verification code') ||
			pageText.includes('请输入验证码') || pageText.includes('已发送') || pageText.includes('sent');
		
		// 检查是否是姓名页面（正常流程）
		const isNamePage = pageText.includes('姓氏') || pageText.includes('名字') || 
			pageText.includes('Full name') || pageText.includes('全名');
		
		// 如果是正常的验证码或姓名页面，不要检查错误
		if (isVerificationPage) {
			return { needsVerification: true, isNamePage: false };
		}
		if (isNamePage) {
			return { needsVerification: false, isNamePage: true };
		}
		
		// 只有在非正常页面时才检查错误关键词
		if (pageText.includes('出了点问题') || pageText.includes('Something went wrong') ||
			pageText.includes('无法创建') || pageText.includes('cannot create') ||
			pageText.includes('不安全的') || pageText.includes('not secure') ||
			pageText.includes('需要电话号码') || pageText.includes('phone number required')) {
			return { error: true, text: document.body.innerText.substring(0, 100) };
		}
		
		// 默认需要验证码
		return { needsVerification: true, isNamePage: false };
	}`)

	if checkResult != nil {
		if checkResult.Value.Get("error").Bool() {
			errText := checkResult.Value.Get("text").String()
			result.Error = fmt.Errorf("页面显示错误: %s...", errText)
			log.Printf("[注册 %d] ❌ %v", threadID, result.Error)
			return result
		}
		needsVerification = checkResult.Value.Get("needsVerification").Bool()
		isNamePage := checkResult.Value.Get("isNamePage").Bool()
		log.Printf("[注册 %d] 页面状态: needsVerification=%v, isNamePage=%v", threadID, needsVerification, isNamePage)
	} else {
		needsVerification = true
	}

	// 处理验证码
	if needsVerification {
		log.Printf("🔐 [注册 %d] 步骤 5/8: 获取验证码...", threadID)
		maxWaitTime := 3 * time.Minute
		var code string
		var codeErr error

		// 使用统一的验证码获取函数
		if isQQImapConfigured() {
			// IMAP邮箱方案：直接获取验证码
			log.Printf("📬 [注册 %d] 使用IMAP邮箱获取验证码 (IMAP邮箱: %s, 目标邮箱: %s)...",
				threadID, appConfig.Email.QQImap.Address, email)
			code, codeErr = getVerificationCode(email, maxWaitTime)
		} else {
			log.Printf("📨 [注册 %d] 使用临时邮箱API获取验证码...", threadID)
			// 临时邮箱方案：原有逻辑
			var emailContent *EmailContent
			startTime := time.Now()

			for time.Since(startTime) < maxWaitTime {
				// 尝试点击重发按钮
				clickResult, _ := page.Eval(`() => {
					// 精确匹配: <span jsname="V67aGc" class="YuMlnb-vQzf8d">重新发送验证码</span>
					const btn = document.querySelector('span[jsname="V67aGc"].YuMlnb-vQzf8d') ||
					            document.querySelector('span.YuMlnb-vQzf8d');
					
					if (btn && btn.textContent.includes('重新发送')) {
						btn.click();
						if (btn.parentElement) btn.parentElement.click();
						return {clicked: true};
					}
					return {clicked: false};
				}`)

				if clickResult != nil && clickResult.Value.Get("clicked").Bool() {
					time.Sleep(1 * time.Second)
				}

				// 快速检查邮件
				emailContent, _ = getVerificationEmailQuick(email, 1, 1)
				if emailContent != nil {
					break
				}
			}

			if emailContent == nil {
				codeErr = fmt.Errorf("无法获取验证码邮件")
			} else {
				code, codeErr = extractVerificationCode(emailContent.Content)
			}
		}

		if codeErr != nil {
			log.Printf("❌ [注册 %d] 获取验证码失败: %v", threadID, codeErr)
			result.Error = codeErr
			return result
		}

		log.Printf("✅ [注册 %d] 获取到验证码: %s", threadID, code)

		// 等待验证码输入框
		log.Printf("✍️ [注册 %d] 步骤 6/8: 输入验证码...", threadID)
		time.Sleep(500 * time.Millisecond)

		// 清空并聚焦输入框
		page.Eval(`() => {
			const inputs = document.querySelectorAll('input');
			if (inputs.length > 0) {
				inputs[0].value = '';
				inputs[0].click();
				inputs[0].focus();
			}
		}`)
		time.Sleep(200 * time.Millisecond)
		log.Printf("⌨️ [注册 %d] 开始输入验证码: %s", threadID, code)
		safeType(page, code, 15)
		log.Printf("✅ [注册 %d] 验证码输入完成", threadID)
		time.Sleep(500 * time.Millisecond)

		// 触发 blur
		page.Eval(`() => {
			const inputs = document.querySelectorAll('input');
			if (inputs.length > 0) {
				inputs[0].blur();
			}
		}`)
		time.Sleep(500 * time.Millisecond)

		for i := 0; i < 5; i++ {
			clickResult, _ := page.Eval(`() => {
				const targets = ['验证', 'Verify', '继续', 'Next', 'Continue'];
				const elements = [
					...document.querySelectorAll('button'),
					...document.querySelectorAll('input[type="submit"]'),
					...document.querySelectorAll('div[role="button"]')
				];

				for (const element of elements) {
					if (!element) continue;
					const style = window.getComputedStyle(element);
					if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') continue;
					if (element.disabled) continue;

					const text = element.textContent ? element.textContent.trim() : '';
					if (targets.some(t => text.includes(t))) {
						element.click();
						return { clicked: true, text: text };
					}
				}
				return { clicked: false };
			}`)

			if clickResult != nil && clickResult.Value.Get("clicked").Bool() {
				break
			}
			time.Sleep(1 * time.Second)
		}

		time.Sleep(2 * time.Second)
	}

	// 填写姓名
	log.Printf("👤 [注册 %d] 步骤 7/8: 填写姓名...", threadID)
	fullName := generateRandomName()
	result.FullName = fullName
	log.Printf("📝 [注册 %d] 生成随机姓名: %s", threadID, fullName)

	time.Sleep(500 * time.Millisecond)

	// 清空并聚焦输入框
	page.Eval(`() => {
		const inputs = document.querySelectorAll('input');
		if (inputs.length > 0) {
			inputs[0].value = '';
			inputs[0].click();
			inputs[0].focus();
		}
	}`)
	time.Sleep(200 * time.Millisecond)

	// 输入姓名
	log.Printf("⌨️ [注册 %d] 开始输入姓名: %s", threadID, fullName)
	safeType(page, fullName, 15)
	log.Printf("✅ [注册 %d] 姓名输入完成", threadID)
	time.Sleep(500 * time.Millisecond)

	// 触发 blur
	page.Eval(`() => {
		const inputs = document.querySelectorAll('input');
		if (inputs.length > 0) {
			inputs[0].blur();
		}
	}`)
	time.Sleep(200 * time.Millisecond)

	// 确认提交姓名
	confirmSubmitted := false
	for i := 0; i < 5; i++ {
		clickResult, _ := page.Eval(`() => {
			const targets = ['同意', 'Confirm', '继续', 'Next', 'Continue', 'I agree'];
			const elements = [
				...document.querySelectorAll('button'),
				...document.querySelectorAll('input[type="submit"]'),
				...document.querySelectorAll('div[role="button"]')
			];

			for (const element of elements) {
				if (!element) continue;
				const style = window.getComputedStyle(element);
				if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') continue;
				if (element.disabled) continue;

				const text = element.textContent ? element.textContent.trim() : '';
				if (targets.some(t => text.includes(t))) {
					element.click();
					return { clicked: true, text: text };
				}
			}

			// 备用：点击第一个可见按钮
			for (const element of elements) {
				if (element && element.offsetParent !== null && !element.disabled) {
					element.click();
					return { clicked: true, text: 'fallback' };
				}
			}
			return { clicked: false };
		}`)

		if clickResult != nil && clickResult.Value.Get("clicked").Bool() {
			confirmSubmitted = true
			break
		}
		time.Sleep(1000 * time.Millisecond)
	}

	if !confirmSubmitted {
		log.Printf("[注册 %d] ⚠️ 未能点击确认按钮，尝试继续", threadID)
	}

	time.Sleep(3 * time.Second)

	// 等待页面稳定
	page.WaitLoad()
	time.Sleep(2 * time.Second)

	// 处理额外步骤（主要是复选框）
	handleAdditionalSteps(page, threadID)

	// 检查并处理管理创建页面
	checkAndHandleAdminPage(page, threadID)

	// 等待更多可能的跳转
	time.Sleep(3 * time.Second)

	// 尝试多次点击可能出现的额外按钮，并等待获取 Authorization
	// 增加到 25 次，每次等待 3 秒
	log.Printf("🔑 [注册 %d] 步骤 8/8: 等待获取 Authorization...", threadID)
	log.Printf("⏳ [注册 %d] 最多尝试 25 次，每次间隔 3 秒...", threadID)

	for i := 0; i < 25; i++ {
		time.Sleep(3 * time.Second)

		// 尝试点击可能出现的额外按钮
		page.Eval(`() => {
			const buttons = document.querySelectorAll('button');
			for (const button of buttons) {
				if (!button) continue;
				const text = button.textContent || '';
				if (text.includes('同意') || text.includes('Confirm') || text.includes('继续') || 
					text.includes('Next') || text.includes('I agree')) {
					if (button.offsetParent !== null && !button.disabled) {
						button.click();
						return true;
					}
				}
			}
			return false;
		}`)

		// 从 URL 提取信息
		info, _ := page.Info()
		if info != nil {
			currentURL := info.URL
			if m := regexp.MustCompile(`/cid/([a-f0-9-]+)`).FindStringSubmatch(currentURL); len(m) > 1 && configID == "" {
				configID = m[1]
				log.Printf("[注册 %d] 从URL提取 configId: %s", threadID, configID)
			}
			if m := regexp.MustCompile(`[?&]csesidx=(\d+)`).FindStringSubmatch(currentURL); len(m) > 1 && csesidx == "" {
				csesidx = m[1]
				log.Printf("[注册 %d] 从URL提取 csesidx: %s", threadID, csesidx)
			}
		}

		// 每 5 次尝试打印一次状态
		if (i+1)%5 == 0 {
			if authorization == "" {
				log.Printf("[注册 %d] ⏳ 等待 Authorization... (%d/25)", threadID, i+1)
			}
		}

		if authorization != "" {
			log.Printf("[注册 %d] ✅ 已获取到 Authorization (第 %d 次检查)", threadID, i+1)
			break
		}
	}

	// 增强的 Authorization 获取逻辑
	if authorization == "" {
		log.Printf("[注册 %d] ⚠️ 仍未获取到 Authorization，尝试主动触发网络请求...", threadID)

		// 尝试导航到主页，触发认证请求
		page.Navigate("https://business.gemini.google/app")
		page.WaitLoad()
		time.Sleep(5 * time.Second)

		// 如果还没有，尝试刷新页面
		if authorization == "" {
			log.Printf("[注册 %d] 尝试刷新页面...", threadID)
			page.Reload()
			page.WaitLoad()
			time.Sleep(5 * time.Second)
		}

		// 尝试从 localStorage 获取
		localStorageAuth, _ := page.Eval(`() => {
			const auth = localStorage.getItem('Authorization') || 
				   localStorage.getItem('authorization') ||
				   localStorage.getItem('auth_token') ||
				   localStorage.getItem('token');
			return auth || ''; // 确保返回字符串而不是 null
		}`)

		if localStorageAuth != nil {
			authStr := localStorageAuth.Value.String()
			// 过滤掉 nil, null, undefined 等无效值
			if authStr != "" && authStr != "<nil>" && authStr != "null" && authStr != "undefined" {
				authorization = authStr
				log.Printf("[注册 %d] 从 localStorage 获取 Authorization", threadID)
			}
		}

		// 从页面源代码中提取
		pageContent, _ := page.Eval(`() => document.body ? document.body.innerHTML : ''`)
		if pageContent != nil && pageContent.Value.String() != "" {
			content := pageContent.Value.String()
			re := regexp.MustCompile(`"authorization"\s*:\s*"([^"]+)"`)
			if matches := re.FindStringSubmatch(content); len(matches) > 1 {
				authorization = matches[1]
				log.Printf("[注册 %d] 从页面内容提取 Authorization", threadID)
			}
		}

		// 从当前 URL 中提取
		info, _ := page.Info()
		if info != nil {
			currentURL := info.URL
			re := regexp.MustCompile(`[?&](?:token|auth)=([^&]+)`)
			if matches := re.FindStringSubmatch(currentURL); len(matches) > 1 {
				authorization = matches[1]
				log.Printf("[注册 %d] 从 URL 提取 Authorization", threadID)
			}
		}
	}

	if authorization == "" {
		log.Printf("❌ [注册 %d] 未能获取 Authorization", threadID)
		result.Error = fmt.Errorf("未能获取 Authorization")
		return result
	}
	log.Printf("✅ [注册 %d] Authorization 获取成功", threadID)

	log.Printf("🍪 [注册 %d] 收集 Cookies...", threadID)
	var resultCookies []Cookie
	cookieMap := make(map[string]bool)

	// 获取当前页面所有 cookie
	cookies, _ := page.Cookies(nil)
	for _, c := range cookies {
		key := c.Name + "|" + c.Domain
		if !cookieMap[key] {
			cookieMap[key] = true
			resultCookies = append(resultCookies, Cookie{
				Name:   c.Name,
				Value:  c.Value,
				Domain: c.Domain,
			})
		}
	}

	// 尝试从特定域名获取更多 cookie
	domains := []string{
		"https://business.gemini.google",
		"https://gemini.google",
		"https://accounts.google.com",
	}
	for _, domain := range domains {
		domainCookies, err := page.Cookies([]string{domain})
		if err == nil {
			for _, c := range domainCookies {
				key := c.Name + "|" + c.Domain
				if !cookieMap[key] {
					cookieMap[key] = true
					resultCookies = append(resultCookies, Cookie{
						Name:   c.Name,
						Value:  c.Value,
						Domain: c.Domain,
					})
				}
			}
		}
	}

	log.Printf("[注册 %d] 获取到 %d 个 Cookie", threadID, len(resultCookies))

	// 检查 Authorization 是否有效
	if authorization == "" || authorization == "<nil>" || authorization == "null" {
		log.Printf("[注册 %d] ⚠️ Authorization 无效或为空，账号可能无法正常使用", threadID)
		authorization = "" // 清空无效值
	} else {
		log.Printf("[注册 %d] ✅ 已获取有效 Authorization", threadID)
	}

	result.Success = true
	result.Authorization = authorization
	result.Cookies = resultCookies
	result.ConfigID = configID
	result.CSESIDX = csesidx

	log.Printf("🎉 [注册 %d] ========== 注册成功 ==========", threadID)
	log.Printf("📋 [注册 %d] 账号信息:", threadID)
	log.Printf("   • 邮箱: %s", email)
	log.Printf("   • 姓名: %s", fullName)
	log.Printf("   • ConfigID: %s", configID)
	log.Printf("   • CSESIDX: %s", csesidx)
	log.Printf("   • Cookies数量: %d", len(resultCookies))
	log.Printf("   • Authorization: %s...", authorization[:min(50, len(authorization))])

	return result
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// SaveBrowserRegisterResult 保存注册结果
func SaveBrowserRegisterResult(result *BrowserRegisterResult, dataDir string) error {
	log.Printf("💾 [保存账号] 开始保存注册结果...")
	log.Printf("📧 [保存账号] 邮箱: %s", result.Email)

	if !result.Success {
		log.Printf("❌ [保存账号] 注册未成功，跳过保存")
		return result.Error
	}

	data := AccountData{
		Email:         result.Email,
		FullName:      result.FullName,
		Authorization: result.Authorization,
		Cookies:       result.Cookies,
		ConfigID:      result.ConfigID,
		CSESIDX:       result.CSESIDX,
		Timestamp:     time.Now().Format(time.RFC3339),
	}

	log.Printf("📋 [保存账号] 账号数据:")
	log.Printf("   • Email: %s", data.Email)
	log.Printf("   • FullName: %s", data.FullName)
	log.Printf("   • ConfigID: %s", data.ConfigID)
	log.Printf("   • CSESIDX: %s", data.CSESIDX)
	log.Printf("   • Cookies数量: %d", len(data.Cookies))
	log.Printf("   • Timestamp: %s", data.Timestamp)

	log.Printf("🔄 [保存账号] 序列化为JSON...")
	jsonData, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		log.Printf("❌ [保存账号] 序列化失败: %v", err)
		return fmt.Errorf("序列化失败: %w", err)
	}
	log.Printf("✅ [保存账号] JSON大小: %d 字节", len(jsonData))

	filename := filepath.Join(dataDir, fmt.Sprintf("%s.json", result.Email))
	log.Printf("💾 [保存账号] 写入文件: %s", filename)

	if err := os.WriteFile(filename, jsonData, 0644); err != nil {
		log.Printf("❌ [保存账号] 写入文件失败: %v", err)
		return fmt.Errorf("写入文件失败: %w", err)
	}

	log.Printf("✅ [保存账号] 账号保存成功: %s", filename)
	return nil
}

// BrowserRefreshResult Cookie刷新结果
type BrowserRefreshResult struct {
	Success         bool
	SecureCookies   []Cookie
	ConfigID        string
	CSESIDX         string
	Authorization   string
	ResponseHeaders map[string]string // 捕获的响应头
	NewCookies      []Cookie          // 从响应头提取的新Cookie
	Error           error
}

func RefreshCookieWithBrowser(acc *Account, headless bool, proxy string) *BrowserRefreshResult {
	result := &BrowserRefreshResult{}
	email := acc.Data.Email

	defer func() {
		if r := recover(); r != nil {
			result.Error = fmt.Errorf("panic: %v", r)
		}
	}()

	// 启动浏览器
	l := launcher.New()
	systemBrowsers := []string{
		"/usr/bin/google-chrome", "/usr/bin/google-chrome-stable",
		"/usr/bin/chromium", "/usr/bin/chromium-browser",
		"/snap/bin/chromium", "/opt/google/chrome/chrome",
		"/usr/lib/chromium/chromium",
		"C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe",
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
	}
	for _, path := range systemBrowsers {
		if _, err := os.Stat(path); err == nil {
			l = l.Bin(path)
			break
		}
	}

	l = l.Headless(headless).
		Set("no-sandbox").
		Set("disable-setuid-sandbox").
		Set("disable-dev-shm-usage").
		Set("disable-gpu").
		Set("disable-software-rasterizer").
		Set("disable-blink-features", "AutomationControlled").
		Set("window-size", "1280,800").
		Set("lang", "zh-CN").
		Set("disable-extensions").
		Set("exclude-switches", "enable-automation").
		Set("disable-infobars").
		Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")

	if proxy != "" {
		l = l.Proxy(proxy)
	}

	url, err := l.Launch()
	if err != nil {
		result.Error = fmt.Errorf("启动浏览器失败: %w", err)
		return result
	}

	browser := rod.New().ControlURL(url)
	if err := browser.Connect(); err != nil {
		result.Error = fmt.Errorf("连接浏览器失败: %w", err)
		return result
	}
	defer browser.Close()

	browser = browser.Timeout(120 * time.Second)

	pages, _ := browser.Pages()
	var page *rod.Page
	if len(pages) > 0 {
		page = pages[0]
	} else {
		page, _ = browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
	}

	page.MustSetViewport(1280, 800, 1, false)
	page.SetUserAgent(&proto.NetworkSetUserAgentOverride{
		UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	})

	// 增强的反检测脚本
	page.Eval(`() => {
		// 删除 webdriver 标识
		Object.defineProperty(navigator, 'webdriver', {
			get: () => undefined
		});
		
		// 修复 Chrome 对象
		window.chrome = {
			runtime: {},
			loadTimes: function() {},
			csi: function() {},
			app: {}
		};
		
		// 修复 Permissions API
		const originalQuery = window.navigator.permissions.query;
		window.navigator.permissions.query = (parameters) => (
			parameters.name === 'notifications' ?
				Promise.resolve({ state: Notification.permission }) :
				originalQuery(parameters)
		);
		
		// 修复 plugins
		Object.defineProperty(navigator, 'plugins', {
			get: () => [1, 2, 3, 4, 5]
		});
		
		// 修复 languages
		Object.defineProperty(navigator, 'languages', {
			get: () => ['zh-CN', 'zh', 'en']
		});
	}`)

	// 监听请求和响应以捕获 authorization 和响应头
	var authorization string
	var configID, csesidx string
	var responseHeadersMu sync.Mutex
	responseHeaders := make(map[string]string)
	var newCookiesFromResponse []Cookie

	// 监听响应以捕获 Set-Cookie 等头信息
	go page.EachEvent(func(e *proto.NetworkResponseReceived) {
		responseHeadersMu.Lock()
		defer responseHeadersMu.Unlock()

		// 获取响应头中的重要信息 - Headers 是 map[string]gson.JSON 类型
		headers := e.Response.Headers
		importantKeys := []string{"set-cookie", "Set-Cookie", "authorization", "Authorization",
			"x-goog-authenticated-user", "X-Goog-Authenticated-User"}

		for _, key := range importantKeys {
			if val, ok := headers[key]; ok {
				str := val.Str()
				if str == "" {
					continue
				}
				responseHeaders[key] = str
				// 解析 Set-Cookie
				if strings.ToLower(key) == "set-cookie" {
					parts := strings.Split(str, ";")
					if len(parts) > 0 {
						nv := strings.SplitN(parts[0], "=", 2)
						if len(nv) == 2 {
							newCookiesFromResponse = append(newCookiesFromResponse, Cookie{
								Name:   strings.TrimSpace(nv[0]),
								Value:  strings.TrimSpace(nv[1]),
								Domain: ".gemini.google",
							})
						}
					}
				}
			}
		}
	})()

	go page.EachEvent(func(e *proto.NetworkRequestWillBeSent) {
		if auth, ok := e.Request.Headers["authorization"]; ok {
			if authStr := auth.String(); authStr != "" {
				authorization = authStr
			}
		}
		reqURL := e.Request.URL
		if m := regexp.MustCompile(`/cid/([a-f0-9-]+)`).FindStringSubmatch(reqURL); len(m) > 1 && configID == "" {
			configID = m[1]
		}
		if m := regexp.MustCompile(`[?&]csesidx=(\d+)`).FindStringSubmatch(reqURL); len(m) > 1 && csesidx == "" {
			csesidx = m[1]
		}
	})()

	// 导航到目标页面
	targetURL := "https://business.gemini.google/"
	page.Navigate(targetURL)
	page.WaitLoad()
	time.Sleep(2 * time.Second)

	// 检查页面状态
	info, _ := page.Info()
	currentURL := ""
	if info != nil {
		currentURL = info.URL
	}
	initialEmailCount := 0
	maxCodeRetries := 3 // 验证码重试次数（必须在goto之前声明）

	// 检查是否已经登录成功（有authorization）
	if authorization != "" {
		log.Printf("[Cookie刷新] [%s] Cookie有效，已自动登录", email)
		goto extractResult
	}

	// 获取实际邮件数量
	initialEmailCount = getEmailCount(email)

	// 检查是否在登录页面需要输入邮箱
	if _, err := page.Timeout(5 * time.Second).Element("input"); err == nil {

		// 输入邮箱 - 先清空再输入
		time.Sleep(500 * time.Millisecond)
		page.Eval(`() => {
			const inputs = document.querySelectorAll('input');
			if (inputs.length > 0) {
				inputs[0].value = '';
				inputs[0].click();
				inputs[0].focus();
			}
		}`)
		time.Sleep(300 * time.Millisecond)
		safeType(page, email, 30)
		time.Sleep(500 * time.Millisecond)
		page.Eval(`() => {
			const inputs = document.querySelectorAll('input');
			if (inputs.length > 0) { inputs[0].blur(); }
		}`)
		time.Sleep(500 * time.Millisecond)

		// 点击继续按钮
		for i := 0; i < 5; i++ {
			clickResult, _ := page.Eval(`() => {
				const targets = ['继续', 'Next', 'Continue', '邮箱'];
				const elements = [...document.querySelectorAll('button'), ...document.querySelectorAll('div[role="button"]')];
				for (const el of elements) {
					if (!el || el.disabled) continue;
					const style = window.getComputedStyle(el);
					if (style.display === 'none' || style.visibility === 'hidden') continue;
					const text = el.textContent ? el.textContent.trim() : '';
					if (targets.some(t => text.includes(t))) { el.click(); return {clicked:true}; }
				}
				return {clicked:false};
			}`)
			if clickResult != nil && clickResult.Value.Get("clicked").Bool() {
				break
			}
			time.Sleep(1 * time.Second)
		}
		time.Sleep(2 * time.Second)
	}
	time.Sleep(3 * time.Second)

	// 验证码重试循环
	for codeRetry := 0; codeRetry < maxCodeRetries; codeRetry++ {
		if codeRetry > 0 {
			log.Printf("[Cookie刷新] [%s] 验证码验证失败，重试 %d/%d", email, codeRetry+1, maxCodeRetries)
			// 点击"重新发送验证码"按钮
			page.Eval(`() => {
				const links = document.querySelectorAll('a, span, button');
				for (const el of links) {
					const text = el.textContent || '';
					if (text.includes('重新发送') || text.includes('Resend')) {
						el.click();
						return true;
					}
				}
				return false;
			}`)
			time.Sleep(2 * time.Second)
			// 更新邮件计数基准
			initialEmailCount = getEmailCount(email)
		}

		var code string
		var codeErr error
		maxWaitTime := 3 * time.Minute

		// 判断是否使用IMAP邮箱（检查邮箱域名是否匹配配置的注册域名）
		useQQImap := isQQImapConfigured() && strings.HasSuffix(email, "@"+appConfig.Email.RegisterDomain)

		if useQQImap {
			// IMAP邮箱方案
			log.Printf("[Cookie刷新] [%s] 使用IMAP邮箱获取验证码 (邮箱: %s)...", email, appConfig.Email.QQImap.Address)
			code, codeErr = getVerificationCode(email, maxWaitTime)
		} else {
			// 临时邮箱方案
			var emailContent *EmailContent
			startTime := time.Now()

			for time.Since(startTime) < maxWaitTime {
				// 快速检查新邮件（只接受数量增加的情况）
				emailContent, _ = getVerificationEmailAfter(email, 1, 1, initialEmailCount)
				if emailContent != nil {
					break
				}
				time.Sleep(2 * time.Second)
			}

			if emailContent == nil {
				codeErr = fmt.Errorf("无法获取验证码邮件")
			} else {
				code, codeErr = extractVerificationCode(emailContent.Content)
			}
		}

		if codeErr != nil {
			if codeRetry == maxCodeRetries-1 {
				result.Error = codeErr
				return result
			}
			continue // 重试
		}

		log.Printf("[Cookie刷新] [%s] 获取到验证码: %s", email, code)

		// 输入验证码
		time.Sleep(500 * time.Millisecond)
		page.Eval(`() => {
			const inputs = document.querySelectorAll('input');
			for (const inp of inputs) { inp.value = ''; }
			if (inputs.length > 0) { inputs[0].click(); inputs[0].focus(); }
		}`)
		time.Sleep(300 * time.Millisecond)
		safeType(page, code, 30)
		time.Sleep(500 * time.Millisecond)

		// 点击验证按钮
		for i := 0; i < 5; i++ {
			clickResult, _ := page.Eval(`() => {
				const targets = ['验证', 'Verify', '继续', 'Next', 'Continue'];
				const els = [...document.querySelectorAll('button'), ...document.querySelectorAll('div[role="button"]')];
				for (const el of els) {
					if (!el || el.disabled) continue;
					const style = window.getComputedStyle(el);
					if (style.display === 'none' || style.visibility === 'hidden') continue;
					const text = el.textContent ? el.textContent.trim() : '';
					if (targets.some(t => text.includes(t))) { el.click(); return {clicked:true}; }
				}
				return {clicked:false};
			}`)
			if clickResult != nil && clickResult.Value.Get("clicked").Bool() {
				break
			}
			time.Sleep(1 * time.Second)
		}
		time.Sleep(2 * time.Second)

		// 检测验证码错误
		hasError, _ := page.Eval(`() => {
			const text = document.body.innerText || '';
			return text.includes('验证码有误') || text.includes('incorrect') || text.includes('wrong code') || text.includes('请重试');
		}`)
		if hasError != nil && hasError.Value.Bool() {
			continue // 重试
		}

		// 验证成功，跳出重试循环
		break
	}
	for i := 0; i < 15; i++ {
		time.Sleep(2 * time.Second)

		// 点击可能出现的确认按钮
		page.Eval(`() => {
			const btns = document.querySelectorAll('button');
			for (const btn of btns) {
				const text = btn.textContent || '';
				if ((text.includes('同意') || text.includes('Confirm') || text.includes('继续') || text.includes('I agree')) && btn.offsetParent !== null && !btn.disabled) {
					btn.click(); return true;
				}
			}
			return false;
		}`)

		// 从URL提取信息
		info, _ := page.Info()
		if info != nil {
			if m := regexp.MustCompile(`/cid/([a-f0-9-]+)`).FindStringSubmatch(info.URL); len(m) > 1 && configID == "" {
				configID = m[1]
			}
			if m := regexp.MustCompile(`[?&]csesidx=(\d+)`).FindStringSubmatch(info.URL); len(m) > 1 && csesidx == "" {
				csesidx = m[1]
			}
		}

		if authorization != "" {
			break
		}
	}

extractResult:
	if authorization == "" {
		result.Error = fmt.Errorf("未能获取 Authorization")
		return result
	}

	// 获取cookies - 合并浏览器cookie和响应头中的cookie
	cookies, _ := page.Cookies(nil)
	cookieMap := make(map[string]Cookie) // 用于去重，后添加的会覆盖先添加的

	// 先添加原有的 cookie（作为基础）
	for _, c := range acc.Data.GetAllCookies() {
		cookieMap[c.Name] = c
	}

	// 再添加浏览器获取的 cookie（会覆盖旧的）
	for _, c := range cookies {
		cookieMap[c.Name] = Cookie{
			Name:   c.Name,
			Value:  c.Value,
			Domain: c.Domain,
		}
	}

	// 最后添加从响应头获取的新 cookie（最高优先级）
	responseHeadersMu.Lock()
	for _, c := range newCookiesFromResponse {
		cookieMap[c.Name] = c
	}
	// 复制响应头
	result.ResponseHeaders = make(map[string]string)
	for k, v := range responseHeaders {
		result.ResponseHeaders[k] = v
	}
	result.NewCookies = newCookiesFromResponse
	responseHeadersMu.Unlock()

	// 转换为数组
	var resultCookies []Cookie
	for _, c := range cookieMap {
		resultCookies = append(resultCookies, c)
	}

	// 从URL提取最终信息
	info, _ = page.Info()
	if info != nil {
		currentURL = info.URL
		if m := regexp.MustCompile(`/cid/([a-f0-9-]+)`).FindStringSubmatch(currentURL); len(m) > 1 && configID == "" {
			configID = m[1]
		}
		if m := regexp.MustCompile(`[?&]csesidx=(\d+)`).FindStringSubmatch(currentURL); len(m) > 1 && csesidx == "" {
			csesidx = m[1]
		}
	}

	result.Success = true
	result.Authorization = authorization
	result.SecureCookies = resultCookies
	result.ConfigID = configID
	result.CSESIDX = csesidx

	log.Printf("[Cookie刷新] ✅ [%s] 刷新成功", email)
	return result
}

// NativeRegisterWorker 原生 Go 注册 worker
func NativeRegisterWorker(id int, dataDirAbs string) {
	log.Printf("🏁 [注册线程 %d] 线程启动，延迟 %d 秒后开始工作", id, id*3)
	time.Sleep(time.Duration(id) * 3 * time.Second)

	taskCount := 0
	for atomic.LoadInt32(&isRegistering) == 1 {
		currentCount := pool.TotalCount()
		targetCount := appConfig.Pool.TargetCount

		if currentCount >= targetCount {
			log.Printf("✅ [注册线程 %d] 已达目标账号数 (%d/%d)，线程退出", id, currentCount, targetCount)
			return
		}

		taskCount++
		log.Printf("🔨 [注册线程 %d] 开始第 %d 次注册任务 (当前进度: %d/%d)", id, taskCount, currentCount, targetCount)

		startTime := time.Now()
		result := RunBrowserRegister(appConfig.Pool.RegisterHeadless, Proxy, id)
		duration := time.Since(startTime)

		if result.Success {
			log.Printf("💾 [注册线程 %d] 保存注册结果到文件...", id)
			if err := SaveBrowserRegisterResult(result, dataDirAbs); err != nil {
				log.Printf("❌ [注册线程 %d] 保存失败 (耗时 %v): %v", id, duration, err)
				registerStats.AddFailed(err.Error())
			} else {
				log.Printf("✅ [注册线程 %d] 保存成功 (耗时 %v)，重新加载账号池", id, duration)
				registerStats.AddSuccess()
				pool.Load(DataDir)
				log.Printf("📊 [注册线程 %d] 当前账号池: 总数=%d, 就绪=%d, 待刷新=%d",
					id, pool.TotalCount(), pool.ReadyCount(), pool.PendingCount())
			}
		} else {
			errMsg := "未知错误"
			if result.Error != nil {
				errMsg = result.Error.Error()
			}
			log.Printf("❌ [注册线程 %d] 注册失败 (耗时 %v): %s", id, duration, errMsg)
			registerStats.AddFailed(errMsg)

			// 根据错误类型决定等待时间
			if strings.Contains(errMsg, "频繁") || strings.Contains(errMsg, "rate") ||
				strings.Contains(errMsg, "timeout") || strings.Contains(errMsg, "连接") {
				waitTime := 10 + id*2
				log.Printf("⏳ [注册线程 %d] 检测到限流/超时错误，等待 %d 秒后重试...", id, waitTime)
				time.Sleep(time.Duration(waitTime) * time.Second)
			} else {
				log.Printf("⏳ [注册线程 %d] 等待 3 秒后继续...", id)
				time.Sleep(3 * time.Second)
			}
		}
	}
	log.Printf("🛑 [注册线程 %d] 线程停止 (共完成 %d 次注册任务)", id, taskCount)
}
