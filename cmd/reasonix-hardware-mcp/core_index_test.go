package main

// board manager 索引源选择的测试:国内网络对 GitHub 系域名不友好,esp32 必须
// 中国镜像优先 + 可环境变量覆盖 + 探测不可达时自动落到下一个候选。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestArduinoCoreURLCandidatesESP32CNFirst(t *testing.T) {
	t.Setenv("REASONIX_ESP32_INDEX_URL", "")
	urls := arduinoCoreURLCandidates("esp32:esp32")
	if len(urls) != 4 {
		t.Fatalf("esp32 应有 4 个候选(jsDelivr×2 + github.io×2), got %v", urls)
	}
	// 前三个都必须是中国版索引(_cn.json,包地址指向国内 CDN)
	for i := 0; i < 3; i++ {
		if !strings.Contains(urls[i], "package_esp32_index_cn.json") {
			t.Errorf("候选 %d 应是中国镜像索引, got %s", i, urls[i])
		}
	}
	if !strings.Contains(urls[0], "jsdelivr") {
		t.Errorf("首选应走 jsDelivr(国内实测唯一可达的索引宿主), got %s", urls[0])
	}
	last := urls[len(urls)-1]
	if !strings.Contains(last, "package_esp32_index.json") || strings.Contains(last, "_cn") {
		t.Errorf("最后兜底应是全球索引, got %s", last)
	}
}

func TestArduinoCoreURLCandidatesEnvOverride(t *testing.T) {
	t.Setenv("REASONIX_ESP32_INDEX_URL", "http://192.168.6.131:9000/package_esp32_index_cn.json")
	urls := arduinoCoreURLCandidates("esp32:esp32")
	if len(urls) != 1 || urls[0] != "http://192.168.6.131:9000/package_esp32_index_cn.json" {
		t.Fatalf("设了 REASONIX_ESP32_INDEX_URL 应只用覆盖值, got %v", urls)
	}
}

func TestArduinoCoreURLCandidatesOtherCores(t *testing.T) {
	if urls := arduinoCoreURLCandidates("arduino:avr"); urls != nil {
		t.Errorf("官方 core 不需要 additional-urls, got %v", urls)
	}
	if urls := arduinoCoreURLCandidates("esp8266:esp8266"); len(urls) != 1 {
		t.Errorf("esp8266 应是单一候选, got %v", urls)
	}
}

func TestArduinoCoreURLCandidatesEnvOverrideOtherCores(t *testing.T) {
	t.Setenv("REASONIX_ESP8266_INDEX_URL", "http://mirror.local/esp8266.json")
	t.Setenv("REASONIX_RP2040_INDEX_URL", "http://mirror.local/rp2040.json")
	if urls := arduinoCoreURLCandidates("esp8266:esp8266"); len(urls) != 1 || urls[0] != "http://mirror.local/esp8266.json" {
		t.Errorf("esp8266 应用环境变量覆盖值, got %v", urls)
	}
	if urls := arduinoCoreURLCandidates("rp2040:rp2040"); len(urls) != 1 || urls[0] != "http://mirror.local/rp2040.json" {
		t.Errorf("rp2040 应用环境变量覆盖值, got %v", urls)
	}
}

func TestCoreIndexOverrideEnvNames(t *testing.T) {
	cases := map[string]string{
		"esp32:esp32":     "REASONIX_ESP32_INDEX_URL",
		"esp8266:esp8266": "REASONIX_ESP8266_INDEX_URL",
		"rp2040:rp2040":   "REASONIX_RP2040_INDEX_URL",
	}
	for core, want := range cases {
		if got := coreIndexOverrideEnv(core); got != want {
			t.Errorf("coreIndexOverrideEnv(%q) = %q, want %q", core, got, want)
		}
	}
}

func TestPickReachableURLFallsBackToSecond(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"packages":[]}`))
	}))
	defer good.Close()

	got := pickReachableURL([]string{bad.URL, good.URL})
	if got != good.URL {
		t.Errorf("首选 503 时应切到第二个候选, got %s", got)
	}
}

func TestPickReachableURLPrefersFirstWhenHealthy(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer good.Close()
	got := pickReachableURL([]string{good.URL, "http://127.0.0.1:1/unreachable"})
	if got != good.URL {
		t.Errorf("首选可达就该用首选, got %s", got)
	}
}

func TestPickReachableURLAllDeadReturnsFirst(t *testing.T) {
	urls := []string{"http://127.0.0.1:1/a", "http://127.0.0.1:1/b"}
	if got := pickReachableURL(urls); got != urls[0] {
		t.Errorf("全不可达应返回首选让 arduino-cli 报具体错, got %s", got)
	}
}
