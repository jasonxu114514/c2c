package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	deviceFile  = "/data/adb/.deviceid"
	wsURL       = "ws://localhost:8080/connect"
	forceDNS    = "223.5.5.5:53"
	retryDelay  = 3 * time.Second
	readTimeout = 3 * time.Second
	pingPeriod  = 2 * time.Second
)

// ------------------ 資料結構 ------------------
type Message struct {
	Type      string `json:"type"`
	CommandID string `json:"command_id"`
	DeviceID  string `json:"device_id,omitempty"`
	Command   string `json:"command,omitempty"`
	Output    string `json:"output,omitempty"`
	Error     string `json:"error,omitempty"`
}

// ------------------ 設備ID處理 ------------------
func getDeviceID() string {
	data, err := os.ReadFile(deviceFile)
	if err != nil {
		// 如果文件不存在，生成新的設備ID
		newID := generateDeviceID()
		os.WriteFile(deviceFile, []byte(newID), 0644)
		return newID
	}
	return strings.TrimSpace(string(data))
}

func generateDeviceID() string {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "unknown"
	}
	return fmt.Sprintf("%s-%d", hostname, time.Now().Unix())
}

// ------------------ 命令執行 ------------------
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
		Type:      "result",
		CommandID: cmdID,
		DeviceID:  deviceID,
		Output:    string(output),
	}
	if err != nil {
		msg.Error = err.Error()
	}
	return msg
}

// ------------------ WebSocket 連接管理 ------------------
type WSClient struct {
	conn     *websocket.Conn
	mu       sync.RWMutex
	deviceID string
	closed   bool
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

	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 10 * time.Second}
			return d.DialContext(ctx, "udp", forceDNS)
		},
	}

	dialer := websocket.Dialer{
		Proxy: http.ProxyFromEnvironment,
		NetDialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
			Resolver:  resolver,
		}).DialContext,
	}

	conn, _, err := dialer.Dial(u.String(), nil)
	if err != nil {
		return err
	}

	w.mu.Lock()
	w.conn = conn
	w.closed = false
	w.mu.Unlock()

	// 設置心跳和超時處理
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
			// 異步執行命令，避免阻塞消息處理
			go func() {
				result := executeCommand(m.Command, m.CommandID, w.deviceID)
				if err := w.sendMessage(result); err != nil {
					fmt.Printf("發送結果錯誤: %v\n", err)
				}
			}()
		}
	}
}

// ------------------ 連接管理器 ------------------
type ConnectionManager struct {
	client    *WSClient
	deviceID  string
	reconnect chan bool
}

func NewConnectionManager(deviceID string) *ConnectionManager {
	return &ConnectionManager{
		deviceID:  deviceID,
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

		// 啟動心跳和監聽
		go cm.client.startPing()
		go cm.client.listen()

		// 等待連接斷開
		<-cm.reconnect

		// 等待3秒後重連
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

// ------------------ 主函數 ------------------
func main() {
	deviceID := getDeviceID()
	fmt.Printf("設備ID: %s\n", deviceID)

	manager := NewConnectionManager(deviceID)

	// 啟動連接監控
	go manager.monitorConnection()

	// 主循環
	manager.run()
}
