
package ws

// Filename: internal/ws/handler.go

import (
	"log"
	"fmt"
	"net/http"
	"strings"
	"time"
	"sync/atomic"
	"encoding/json"

	"github.com/gorilla/websocket"
)

// Heartbeat and timeout settings
const (
	writeWait  = 5 * time.Second     // max time to complete a write
	pongWait   = 30 * time.Second    // if we don't get a pong in 30s, time out
	pingPeriod = (pongWait * 9) / 10 // send pings at ~90% of pongWait (e.g., 27s)
)

// Only allow pages served from this origin to connect
var allowedOrigins = []string{
	"http://localhost:4000",
}

func originAllowed(o string) bool {
	if o == "" {
		return false
	}
	for _, a := range allowedOrigins {
		if strings.EqualFold(o, a) {
			return true
		}
	}
	return false
}

// The upgrader object is used when we need to upgrade from HTTP to RFC 6455
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		ok := originAllowed(origin)
		if !ok {
			log.Printf("blocked cross-origin websocket: Origin=%q Path=%s", origin, r.URL.Path)
		}
		return ok
	},
	Error: func(w http.ResponseWriter, r *http.Request, status int, reason error) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
	},
}

// reverseString returns a new string which is the Unicode-aware reversal of s.
func reverseString(s string) string {
    runes := []rune(s)
    for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
        runes[i], runes[j] = runes[j], runes[i]
    }
    return string(runes)
}

var messageCounter uint64

// CommandRequest represents an incoming JSON command.
type CommandRequest struct {
    Command string  `json:"command"`
    A       float64 `json:"a"`
    B       float64 `json:"b"`
}

// CommandResponse represents the JSON response for a command.
type CommandResponse struct {
    Result  float64 `json:"result"`
    Command string  `json:"command"`
    Error   string  `json:"error,omitempty"`
	History []string `json:"history,omitempty"`
}

// processCommand parses a JSON command payload and returns a JSON response.
func processCommand(payload []byte) ([]byte, error) {
    var req CommandRequest
    if err := json.Unmarshal(payload, &req); err != nil {
        // return a JSON error response
        resp := CommandResponse{Command: "", Error: fmt.Sprintf("invalid JSON: %v", err)}
        b, _ := json.Marshal(resp)
        return b, fmt.Errorf("invalid JSON: %w", err)
    }

    var res CommandResponse
    res.Command = req.Command

    switch strings.ToLower(req.Command) {
    case "add":
        res.Result = req.A + req.B
    case "subtract":
        res.Result = req.A - req.B
    case "multiply":
        res.Result = req.A * req.B
    case "divide":
        if req.B == 0 {
            res.Error = "division by zero"
        } else {
            res.Result = req.A / req.B
        }
    default:
        res.Error = fmt.Sprintf("unknown command: %s", req.Command)
    }

    b, err := json.Marshal(res)
    if err != nil {
        return nil, err
    }
    return b, nil
}


// Attempt to upgrade from HTTP to RFC 6455
func HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Has to be an HTTP GET request
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Upgrade the connection from HTTP to RFC 6455
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade error: %v", err)
		return
	}
	defer conn.Close()

	log.Printf("connection opened from %s", r.RemoteAddr)

	// Limit message size
	conn.SetReadLimit(1024 * 4)

	// PING / PONG SETUP

	// Idle timeout window starts now: must receive a pong within pongWait
	_ = conn.SetReadDeadline(time.Now().Add(pongWait))

	// On each pong, extend the read deadline again
	conn.SetPongHandler(func(appData string) error {
		_ = conn.SetReadDeadline(time.Now().Add(pongWait))
		log.Printf("pong from %s (data=%q)", r.RemoteAddr, appData)
		return nil
	})

	// Start a goroutine that sends pings every pingPeriod
	done := make(chan struct{})
	ticker := time.NewTicker(pingPeriod)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				// Send a ping; if this fails, the read loop will notice soon
				_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait)); err != nil {
					log.Printf("ping write error: %v", err)
					return
				}
				log.Printf("ping → %s", r.RemoteAddr)
			case <-done:
				return
			}
		}
	}()

	// Read/Echo loop

	// per-connection state: recent message timestamps for rate limiting
    var timestamps []time.Time
	//command history
	var lastCommands []string

    const (
        rateLimitWindow = time.Minute
        rateLimitMax    = 10
		historyMax      = 5
    )

	for {
		msgType, payload, err := conn.ReadMessage()
		if err != nil {
			// This error will be:
			//  - a timeout (no pong in time), or
			//  - a normal close, or
			//  - some other read error
			log.Printf("read error (timeout/close): %v", err)

			// Try to send a graceful close so the client can see 1000 instead of 1006
			_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
			_ = conn.WriteControl(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, "idle timeout"),
				time.Now().Add(writeWait),
			)

			break
		}

		// We successfully read a message; normal traffic also keeps the connection alive.
		// Note: the pong handler also updates the read deadline on pongs.

		// Echo back text messages
		if msgType == websocket.TextMessage {
			now := time.Now()

            // prune timestamps older than rateLimitWindow
            j := 0
            for _, ts := range timestamps {
                if now.Sub(ts) < rateLimitWindow {
                    timestamps[j] = ts
                    j++
                }
            }
            timestamps = timestamps[:j]

            if len(timestamps) >= rateLimitMax {
                // Too many messages: notify client and skip processing
                _ = conn.SetWriteDeadline(time.Now().Add(writeWait))
                warn := fmt.Sprintf("[RateLimit] too many messages - max %d per %s", rateLimitMax, rateLimitWindow)
                if err := conn.WriteMessage(websocket.TextMessage, []byte(warn)); err != nil {
                    log.Printf("write error (rate warn): %v", err)
                    break
                }
                continue
            }

            // record this message
            timestamps = append(timestamps, now)


			//increment message counter
			count := atomic.AddUint64(&messageCounter, 1)

			//process JSON command first 
			payloadStr := string(payload)
            trimmed := strings.TrimSpace(payloadStr)
            if len(trimmed) > 0 && trimmed[0] == '{' {

				// Inspect JSON to check for HISTORY command and record history for other commands
                var req CommandRequest
                if err := json.Unmarshal([]byte(trimmed), &req); err != nil {
                    // send back JSON error response
                    resp := CommandResponse{Command: "", Error: fmt.Sprintf("invalid JSON: %v", err)}
                    b, _ := json.Marshal(resp)
                    _ = conn.SetWriteDeadline(time.Now().Add(writeWait))
                    if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
                        log.Printf("write error: %v", err)
                        break
                    }
                    continue
                }

                if strings.EqualFold(req.Command, "history") {
                    // return history for this connection
                    resp := CommandResponse{Command: "history", History: lastCommands}
                    b, _ := json.Marshal(resp)
                    _ = conn.SetWriteDeadline(time.Now().Add(writeWait))
                    if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
                        log.Printf("write error: %v", err)
                        break
                    }
                    continue
                }

                // record this command payload in per-connection history
                lastCommands = append(lastCommands, trimmed)
                if len(lastCommands) > historyMax {
                    lastCommands = lastCommands[len(lastCommands)-historyMax:]
                }

                respBytes, perr := processCommand([]byte(trimmed))
                _ = conn.SetWriteDeadline(time.Now().Add(writeWait))
                if perr != nil {
                    log.Printf("processCommand error: %v", perr)
                }
                if err := conn.WriteMessage(websocket.TextMessage, respBytes); err != nil {
                    log.Printf("write error: %v", err)
                    break
                }
                continue
            }

			// Plain-text HISTORY command handling
            if strings.EqualFold(strings.TrimSpace(payloadStr), "HISTORY") {
                resp := CommandResponse{Command: "history", History: lastCommands}
                b, _ := json.Marshal(resp)
                _ = conn.SetWriteDeadline(time.Now().Add(writeWait))
                if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
                    log.Printf("write error: %v", err)
                    break
                }
                continue
            }

			// Prepare response body depending on command
            var respBody string
            if strings.HasPrefix(payloadStr, "REVERSE:") {
                body := strings.TrimPrefix(payloadStr, "REVERSE:")
                respBody = reverseString(body)
            } else if strings.HasPrefix(payloadStr, "UPPER:") {
                body := strings.TrimPrefix(payloadStr, "UPPER:")
                respBody = strings.ToUpper(body)
            } else {
                respBody = payloadStr
            }

			// Format response with message number
            out := fmt.Sprintf("[Msg #%d] %s", count, respBody)

            _ = conn.SetWriteDeadline(time.Now().Add(writeWait))
            if err := conn.WriteMessage(websocket.TextMessage, []byte(out)); err != nil {
                log.Printf("write error: %v", err)
                break
            }
        }
    }


	// Stop the ping goroutine
	close(done)

	log.Printf("connection closed from %s", r.RemoteAddr)
}
