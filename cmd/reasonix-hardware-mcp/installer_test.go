package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// 真实网络下载测试:默认跳过,设 REASONIX_HARDWARE_NET_TEST=1 才跑。
// 验证 installArduinoCLI 能为本机下对、解压出一个能跑的 arduino-cli。
func TestInstallArduinoCLIRealDownload(t *testing.T) {
	if os.Getenv("REASONIX_HARDWARE_NET_TEST") == "" {
		t.Skip("设 REASONIX_HARDWARE_NET_TEST=1 跑真实网络下载测试")
	}
	dir := t.TempDir()
	t.Setenv("REASONIX_TOOLS_DIR", dir)
	target, err := installArduinoCLI(180 * time.Second)
	if err != nil {
		t.Fatalf("下载安装失败: %v", err)
	}
	if filepath.Dir(target) != filepath.Join(dir, "bin") {
		t.Errorf("应装到托管 bin 目录, got %s", target)
	}
	out, err := exec.Command(target, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("装好的 arduino-cli 跑不起来: %v\n%s", err, out)
	}
	if !strings.Contains(strings.ToLower(string(out)), "arduino-cli") {
		t.Fatalf("version 输出不像 arduino-cli: %s", out)
	}
	t.Logf("OK 下载并运行成功: %s\n%s", target, strings.TrimSpace(string(out)))
}

func TestArduinoCLIDownloadURL(t *testing.T) {
	url, isZip, err := arduinoCLIDownloadURL()
	if err != nil {
		t.Fatalf("当前平台 %s/%s 应支持: %v", runtime.GOOS, runtime.GOARCH, err)
	}
	if !strings.HasPrefix(url, "https://downloads.arduino.cc/arduino-cli/arduino-cli_latest_") {
		t.Errorf("URL 前缀不对: %s", url)
	}
	if isZip != (runtime.GOOS == "windows") {
		t.Errorf("isZip=%v 应与 windows(%s) 一致", isZip, runtime.GOOS)
	}
	if isZip && !strings.HasSuffix(url, ".zip") {
		t.Errorf("Windows 应是 .zip: %s", url)
	}
	if !isZip && !strings.HasSuffix(url, ".tar.gz") {
		t.Errorf("非 Windows 应是 .tar.gz: %s", url)
	}
}

func TestManagedToolsBinHonorsEnvOverride(t *testing.T) {
	override := filepath.Join(t.TempDir(), "td")
	t.Setenv("REASONIX_TOOLS_DIR", override)
	got := managedToolsBin()
	if filepath.Base(got) != "bin" {
		t.Errorf("应以 bin 结尾: %s", got)
	}
	if !strings.HasPrefix(got, override) {
		t.Errorf("应在覆盖目录下: %s", got)
	}
}

func TestEnsureManagedToolsOnPathIdempotent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("REASONIX_TOOLS_DIR", dir)
	t.Setenv("PATH", filepath.Join("/usr", "bin"))
	want := filepath.Join(dir, "bin")

	ensureManagedToolsOnPath()
	p1 := os.Getenv("PATH")
	if !strings.HasPrefix(p1, want+string(os.PathListSeparator)) {
		t.Fatalf("应把 %s 加到 PATH 头部, got %q", want, p1)
	}
	ensureManagedToolsOnPath() // 再调一次应幂等
	if p2 := os.Getenv("PATH"); p2 != p1 {
		t.Fatalf("幂等失败: %q -> %q", p1, p2)
	}
}

func TestExtractTarGzFile(t *testing.T) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	writeTarEntry(t, tw, "LICENSE.txt", "license decoy") // 干扰项,不应被选中
	writeTarEntry(t, tw, "arduino-cli", "#!/bin/sh\necho fake-cli\n")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(t.TempDir(), "arduino-cli")
	if err := extractTarGzFile(buf.Bytes(), "arduino-cli", target); err != nil {
		t.Fatalf("extractTarGzFile: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "fake-cli") {
		t.Fatalf("抽到的内容不对: %q", got)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(target)
		if info.Mode()&0o100 == 0 {
			t.Errorf("解压出的二进制应可执行, mode=%v", info.Mode())
		}
	}
}

func TestExtractTarGzFileMissingEntry(t *testing.T) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	writeTarEntry(t, tw, "README.md", "no cli here")
	tw.Close()
	gw.Close()
	target := filepath.Join(t.TempDir(), "arduino-cli")
	if err := extractTarGzFile(buf.Bytes(), "arduino-cli", target); err == nil {
		t.Fatal("缺少目标文件时应报错")
	}
}

func TestExtractZipFile(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("arduino-cli.exe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("MZ fake exe body")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(t.TempDir(), "arduino-cli.exe")
	if err := extractZipFile(buf.Bytes(), "arduino-cli.exe", target); err != nil {
		t.Fatalf("extractZipFile: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "MZ fake exe body" {
		t.Fatalf("抽到的内容不对: %q", got)
	}
}

func writeTarEntry(t *testing.T, tw *tar.Writer, name, body string) {
	t.Helper()
	hdr := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
}
