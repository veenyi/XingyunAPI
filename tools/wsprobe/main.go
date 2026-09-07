// Command wsprobe drives the dashboard's web SSH websocket and dumps the
// terminal byte stream, so the bridge can be verified without a browser.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	url := flag.String("url", "", "ws://.../api/devcloud/ws/ssh?project=..&account=..&token=..")
	cols := flag.Int("cols", 100, "terminal columns")
	rows := flag.Int("rows", 30, "terminal rows")
	wait := flag.Duration("wait", 20*time.Second, "how long to read after connect")
	send := flag.String("send", "", "command to type once connected")
	flag.Parse()

	conn, resp, err := websocket.DefaultDialer.Dial(*url, nil)
	if err != nil {
		if resp != nil {
			fmt.Printf("dial failed: HTTP %d: %v\n", resp.StatusCode, err)
		} else {
			fmt.Printf("dial failed: %v\n", err)
		}
		os.Exit(1)
	}
	defer conn.Close()
	fmt.Println("WS_OPEN")

	conn.WriteMessage(websocket.TextMessage,
		[]byte(fmt.Sprintf(`{"type":"resize","cols":%d,"rows":%d}`, *cols, *rows)))

	if *send != "" {
		go func() {
			time.Sleep(3 * time.Second)
			conn.WriteMessage(websocket.BinaryMessage, []byte(*send+"\r"))
		}()
	}

	deadline := time.Now().Add(*wait)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		mt, data, err := conn.ReadMessage()
		if err != nil {
			fmt.Printf("\n[read end: %v]\n", err)
			return
		}
		if mt == websocket.TextMessage {
			fmt.Printf("\n[text] %s\n", data)
			continue
		}
		os.Stdout.Write(data)
	}
	fmt.Println("\n[wait window elapsed]")
}
