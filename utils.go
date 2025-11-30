package main

import (
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"time"
)

// ==================== HTTP 客户端 ====================

var httpClient *http.Client

func newHTTPClient() *http.Client {
	transport := &http.Transport{
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		MaxConnsPerHost:     50,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  false,
		ForceAttemptHTTP2:   true,
	}

	if Proxy != "" {
		proxyURL, err := url.Parse(Proxy)
		if err == nil {
			transport.Proxy = http.ProxyURL(proxyURL)
		}
	}

	return &http.Client{
		Transport: transport,
		Timeout:   1800 * time.Second,
	}
}

func initHTTPClient() {
	httpClient = newHTTPClient()
	if Proxy != "" {
		log.Printf("✅ 使用代理: %s", Proxy)
	}
}

// ==================== 工具函数 ====================

// readResponseBody 读取响应体，自动处理gzip
func readResponseBody(resp *http.Response) ([]byte, error) {
	var reader io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gzReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		defer gzReader.Close()
		reader = gzReader
	}
	return io.ReadAll(reader)
}

// parseNDJSON 解析NDJSON格式数据
func parseNDJSON(data []byte) []map[string]interface{} {
	var result []map[string]interface{}
	lines := bytes.Split(data, []byte("\n"))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var obj map[string]interface{}
		if err := json.Unmarshal(line, &obj); err == nil {
			result = append(result, obj)
		}
	}
	return result
}

func parseIncompleteJSONArray(data []byte) []map[string]interface{} {
	var result []map[string]interface{}
	if err := json.Unmarshal(data, &result); err == nil {
		return result
	}

	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		if trimmed[len(trimmed)-1] != ']' {
			lastBrace := bytes.LastIndex(trimmed, []byte("}"))
			if lastBrace > 0 {
				fixed := append(trimmed[:lastBrace+1], ']')
				if err := json.Unmarshal(fixed, &result); err == nil {
					log.Printf("⚠️ JSON 数组不完整，已修复")
					return result
				}
			}
		}
	}
	return nil
}
