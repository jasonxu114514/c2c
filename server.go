package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/chzyer/readline"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	ColorReset  = "\033[0m"
	ColorRed    = "\033[31m"
	ColorGreen  = "\033[32m"
	ColorYellow = "\033[33m"
	ColorBlue   = "\033[34m"
	ColorCyan   = "\033[36m"
	ColorWhite  = "\033[37m"
)

const (
	PingPeriod         = 2 * time.Second
	ReadTimeout        = 3 * time.Second
	ConnectionTimeout  = 5 * time.Second
	CleanupInterval    = 3 * time.Second
	CommandHoldTimeout = 30 * time.Second
	LogBufferSize      = 1000 // 日誌緩衝區大小
)

type Message struct {
	Type      string `json:"type"`
	CommandID string `json:"command_id"`
	DeviceID  string `json:"device_id,omitempty"`
	Command   string `json:"command,omitempty"`
	Output    string `json:"output,omitempty"`
	Error     string `json:"error,omitempty"`
}

type DeviceConnection struct {
	Conn        *websocket.Conn
	LastSeen    time.Time
	DeviceID    string
	mu          sync.RWMutex
	closed      bool
}

type HoldCommand struct {
	Message   Message
	TargetID  string
	IssueTime time.Time
	HoldTimer *time.Timer
}

// ------------------ 日誌系統 ------------------
type LogEntry struct {
	Timestamp time.Time
	Type      string // "connect", "disconnect", "command_success", "command_failed", "cleanup"
	DeviceID  string
	CommandID string
	Command   string
	Message   string
}

type Logger struct {
	mu     sync.RWMutex
	entries []LogEntry
	file   *os.File
}

func NewLogger() *Logger {
	logger := &Logger{
		entries: make([]LogEntry, 0, LogBufferSize),
	}
	
	// 創建日誌文件
	os.MkdirAll("logs", 0755)
	file, err := os.OpenFile("logs/server.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err == nil {
		logger.file = file
	}
	
	return logger
}

func (l *Logger) Log(logType, deviceID, commandID, command, message string) {
	entry := LogEntry{
		Timestamp: time.Now(),
		Type:      logType,
		DeviceID:  deviceID,
		CommandID: commandID,
		Command:   command,
		Message:   message,
	}

	l.mu.Lock()
	l.entries = append(l.entries, entry)
	// 保持緩衝區大小
	if len(l.entries) > LogBufferSize {
		l.entries = l.entries[len(l.entries)-LogBufferSize:]
	}
	l.mu.Unlock()

	// 寫入文件
	if l.file != nil {
		timestamp := entry.Timestamp.Format("2006-01-02 15:04:05")
		logLine := fmt.Sprintf("[%s] %s", timestamp, l.formatLogLine(entry))
		l.file.WriteString(logLine + "\n")
		l.file.Sync()
	}
}

func (l *Logger) formatLogLine(entry LogEntry) string {
	var typeStr string
	switch entry.Type {
	case "connect":
		typeStr = ColorGreen + "CONNECT" + ColorReset
	case "disconnect":
		typeStr = ColorRed + "DISCONNECT" + ColorReset
	case "command_success":
		typeStr = ColorGreen + "SUCCESS" + ColorReset
	case "command_failed":
		typeStr = ColorRed + "FAILED" + ColorReset
	case "cleanup":
		typeStr = ColorYellow + "CLEANUP" + ColorReset
	default:
		typeStr = entry.Type
	}

	deviceInfo := ""
	if entry.DeviceID != "" {
		deviceInfo = fmt.Sprintf(" Device: %s", entry.DeviceID)
	}

	commandInfo := ""
	if entry.Command != "" {
		commandInfo = fmt.Sprintf(" Command: %s", entry.Command)
	}

	messageInfo := ""
	if entry.Message != "" {
		messageInfo = fmt.Sprintf(" Message: %s", entry.Message)
	}

	return fmt.Sprintf("%s%s%s%s", typeStr, deviceInfo, commandInfo, messageInfo)
}

func (l *Logger) GetRecentLogs(count int) []LogEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if count > len(l.entries) {
		count = len(l.entries)
	}
	return l.entries[len(l.entries)-count:]
}

func (l *Logger) Close() {
	if l.file != nil {
		l.file.Close()
	}
}

// ------------------ 連接池 ------------------
type ConnectionPool struct {
	mu           sync.RWMutex
	conns        map[string]*DeviceConnection
	heldCommands map[string]*HoldCommand
	results      []Message
	logger       *Logger
}

func NewConnectionPool() *ConnectionPool {
	return &ConnectionPool{
		conns:        make(map[string]*DeviceConnection),
		heldCommands: make(map[string]*HoldCommand),
		results:      make([]Message, 0),
		logger:       NewLogger(),
	}
}

// ------------------ DeviceConnection 方法 ------------------
func (dc *DeviceConnection) isClosed() bool {
	dc.mu.RLock()
	defer dc.mu.RUnlock()
	return dc.closed
}

func (dc *DeviceConnection) close() {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	
	if !dc.closed && dc.Conn != nil {
		dc.Conn.Close()
		dc.closed = true
	}
}

func (dc *DeviceConnection) sendMessage(msg Message) error {
	dc.mu.RLock()
	defer dc.mu.RUnlock()
	
	if dc.closed || dc.Conn == nil {
		return fmt.Errorf("connection closed")
	}
	
	dc.Conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return dc.Conn.WriteJSON(msg)
}

func (dc *DeviceConnection) updateLastSeen() {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	dc.LastSeen = time.Now()
}

func (dc *DeviceConnection) startPing() {
	ticker := time.NewTicker(PingPeriod)
	defer ticker.Stop()

	for range ticker.C {
		if dc.isClosed() {
			return
		}

		dc.mu.RLock()
		conn := dc.Conn
		dc.mu.RUnlock()

		if conn != nil {
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				dc.close()
				return
			}
		}
	}
}

// ------------------ CLI 顯示 ------------------
func redrawCLI() {
	rl.Refresh()
	connPool.mu.RLock()
	defer connPool.mu.RUnlock()

	// 顯示在線設備
	fmt.Println(ColorCyan + "\n=== 綜合命令服務器控制台 ===" + ColorReset)
	fmt.Println(ColorYellow + "--- 在線設備 ---" + ColorReset)
	if len(connPool.conns) == 0 {
		fmt.Println("（無設備連線）")
	} else {
		for id, c := range connPool.conns {
			since := time.Since(c.LastSeen).Truncate(time.Second)
			status := ColorGreen + "[在線]" + ColorReset
			if since > ConnectionTimeout {
				status = ColorRed + "[超時]" + ColorReset
			}
			fmt.Printf("%s %s (%s前活動)\n", status, id, since)
		}
	}
	fmt.Println(ColorYellow + fmt.Sprintf("--- 在線設備數量: %d ---", len(connPool.conns)) + ColorReset)

	// 顯示最近命令結果
	fmt.Println(ColorCyan + "--- 最近命令結果 ---" + ColorReset)
	if len(connPool.results) == 0 {
		fmt.Println("（暫無結果）")
	} else {
		for _, res := range connPool.results {
			fmt.Println(ColorGreen + "==============================================" + ColorReset)
			fmt.Printf("%s✅ [%s] 命令ID: %s%s\n", ColorGreen, time.Now().Format("15:04:05"), res.CommandID, ColorReset)
			fmt.Printf("%s👉 執行設備: %s%s\n", ColorWhite, res.DeviceID, ColorReset)
			fmt.Printf("%s👉 執行命令: %s%s\n", ColorWhite, res.Command, ColorReset)
			if res.Error != "" {
				fmt.Printf("%s🚨 執行錯誤: %s%s\n", ColorRed, res.Error, ColorReset)
			}
			fmt.Printf("%s📜 命令輸出:%s\n--- START OUTPUT ---\n%s\n--- END OUTPUT ---\n", ColorYellow, ColorReset, strings.TrimSpace(res.Output))
			fmt.Println(ColorGreen + "==============================================" + ColorReset)
		}
	}

	// 顯示最近日誌摘要
	recentLogs := connPool.logger.GetRecentLogs(5)
	if len(recentLogs) > 0 {
		fmt.Println(ColorCyan + "--- 最近日誌摘要 ---" + ColorReset)
		for _, log := range recentLogs {
			fmt.Printf("[%s] %s\n", log.Timestamp.Format("15:04:05"), connPool.logger.formatLogLine(log))
		}
	}

	rl.Refresh()
}

// ------------------ WebSocket 處理 ------------------
var upgrader = &websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func handleConnections(w http.ResponseWriter, r *http.Request) {
	deviceID := r.URL.Query().Get("device_id")
	if deviceID == "" {
		http.Error(w, "缺少 device_id", http.StatusBadRequest)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	deviceConn := &DeviceConnection{
		Conn:     conn,
		LastSeen: time.Now(),
		DeviceID: deviceID,
	}

	// 添加到連接池
	connPool.AddConn(deviceID, deviceConn)
	defer connPool.RemoveConn(deviceID)
	defer deviceConn.close()

	// 記錄連接日誌
	connPool.logger.Log("connect", deviceID, "", "", "設備連接成功")

	// 設置讀超時
	conn.SetReadLimit(512000)
	conn.SetReadDeadline(time.Now().Add(ReadTimeout))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(ReadTimeout))
		deviceConn.updateLastSeen()
		return nil
	})

	// 啟動心跳
	go deviceConn.startPing()
	
	// 處理暫存命令
	connPool.processHeldCommands(deviceID, deviceConn)

	// 消息處理循環
	for {
		var msg Message
		if err := conn.ReadJSON(&msg); err != nil {
			break
		}
		deviceConn.updateLastSeen()
		if msg.Type == "result" {
			connPool.addResult(msg)
			// 記錄命令執行結果日誌
			if msg.Error != "" {
				connPool.logger.Log("command_failed", msg.DeviceID, msg.CommandID, msg.Command, msg.Error)
			} else {
				connPool.logger.Log("command_success", msg.DeviceID, msg.CommandID, msg.Command, "命令執行成功")
			}
		}
	}
}

// ------------------ ConnectionPool 方法 ------------------
func (p *ConnectionPool) AddConn(deviceID string, conn *DeviceConnection) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.conns[deviceID] = conn
}

func (p *ConnectionPool) RemoveConn(deviceID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if conn, exists := p.conns[deviceID]; exists {
		conn.close()
		delete(p.conns, deviceID)
		// 記錄斷開日誌
		p.logger.Log("disconnect", deviceID, "", "", "設備斷開連接")
	}
}

func (p *ConnectionPool) addResult(msg Message) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.results = append(p.results, msg)
	if len(p.results) > 2 {
		p.results = p.results[len(p.results)-2:]
	}
}

// ------------------ 超時連接清理 ------------------
func (p *ConnectionPool) cleanupStaleConnections() {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	var toRemove []string

	for deviceID, conn := range p.conns {
		if now.Sub(conn.LastSeen) > ConnectionTimeout {
			toRemove = append(toRemove, deviceID)
			p.logger.Log("cleanup", deviceID, "", "", 
				fmt.Sprintf("清理超時連接 (最後活動: %v前)", now.Sub(conn.LastSeen)))
		}
	}

	for _, deviceID := range toRemove {
		if conn, exists := p.conns[deviceID]; exists {
			conn.close()
			delete(p.conns, deviceID)
		}
	}
}

// ------------------ 命令處理 ------------------
func (p *ConnectionPool) tryExecuteOrHold(msg Message) {
	p.mu.Lock()
	defer p.mu.Unlock()
	
	targetID := msg.DeviceID
	if conn, ok := p.conns[targetID]; ok {
		if err := conn.sendMessage(msg); err != nil {
			// 發送失敗，轉為暫存命令
			p.holdCommand(msg, targetID)
		} else {
			// 記錄命令發送日誌
			p.logger.Log("command_sent", targetID, msg.CommandID, msg.Command, "命令已發送到設備")
		}
	} else {
		p.holdCommand(msg, targetID)
		p.logger.Log("command_hold", targetID, msg.CommandID, msg.Command, "設備不在線，命令暫存")
	}
}

func (p *ConnectionPool) holdCommand(msg Message, targetID string) {
	hold := &HoldCommand{
		Message:   msg,
		TargetID:  targetID,
		IssueTime: time.Now(),
	}
	hold.HoldTimer = time.AfterFunc(CommandHoldTimeout, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		delete(p.heldCommands, msg.CommandID)
		p.logger.Log("command_expired", targetID, msg.CommandID, msg.Command, "暫存命令超時過期")
	})
	p.heldCommands[msg.CommandID] = hold
}

func (p *ConnectionPool) SendCommand(msg Message) {
	if msg.DeviceID == "" {
		// 廣播命令
		p.mu.RLock()
		defer p.mu.RUnlock()
		for deviceID, conn := range p.conns {
			if err := conn.sendMessage(msg); err == nil {
				p.logger.Log("broadcast_sent", deviceID, msg.CommandID, msg.Command, "廣播命令已發送")
			}
		}
	} else {
		p.tryExecuteOrHold(msg)
	}
}

func (p *ConnectionPool) processHeldCommands(deviceID string, conn *DeviceConnection) {
	p.mu.Lock()
	defer p.mu.Unlock()
	
	for id, cmd := range p.heldCommands {
		if cmd.TargetID == deviceID {
			if err := conn.sendMessage(cmd.Message); err == nil {
				cmd.HoldTimer.Stop()
				delete(p.heldCommands, id)
				p.logger.Log("command_delivered", deviceID, cmd.Message.CommandID, cmd.Message.Command, "暫存命令已送達設備")
			}
		}
	}
}

// ------------------ CLI Loop ------------------
func cliLoop() {
	for {
		rl.SetPrompt(ColorBlue + "[CLI] 輸入命令: " + ColorReset)
		line, err := rl.Readline()
		if err != nil {
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		rl.SetPrompt(ColorBlue + "[CLI] 輸入 DeviceID (空為廣播): " + ColorReset)
		devID, _ := rl.Readline()
		devID = strings.TrimSpace(devID)

		msg := Message{
			Type:      "command",
			CommandID: uuid.New().String(),
			Command:   line,
			DeviceID:  devID,
		}
		connPool.SendCommand(msg)
	}
}

// ------------------ 自動刷新 CLI ------------------
func autoRefreshCLI() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for range t.C {
		redrawCLI()
	}
}

// ------------------ 定期清理 ------------------
func startCleanupRoutine() {
	ticker := time.NewTicker(CleanupInterval)
	defer ticker.Stop()
	
	for range ticker.C {
		connPool.cleanupStaleConnections()
	}
}

// ------------------ 全局變量 ------------------
var (
	connPool = NewConnectionPool()
	rl       *readline.Instance
)

// ------------------ Main ------------------
func main() {
	var err error
	rl, err = readline.NewEx(&readline.Config{
		HistoryFile:     "/tmp/cli_history.tmp",
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
	})
	if err != nil {
		panic(err)
	}
	defer rl.Close()
	defer connPool.logger.Close()

	// 啟動各種協程
	go cliLoop()
	go autoRefreshCLI()
	go startCleanupRoutine()

	// 設置HTTP路由
	http.HandleFunc("/connect", handleConnections)
	port := ":8080"

	fmt.Printf("%s🚀 服務器啟動中... 監聽 %s%s\n", ColorGreen, port, ColorReset)
	fmt.Printf("%s⏰ 連接超時設置: %v%s\n", ColorYellow, ConnectionTimeout, ColorReset)
	fmt.Printf("%s🧹 清理間隔: %v%s\n", ColorYellow, CleanupInterval, ColorReset)
	fmt.Printf("%s📝 日誌文件: logs/server.log%s\n", ColorYellow, ColorReset)
	
	redrawCLI()
	
	if err := http.ListenAndServe(port, nil); err != nil {
		fmt.Printf("%s服務器啟動失敗: %v%s\n", ColorRed, err, ColorReset)
	}
}
