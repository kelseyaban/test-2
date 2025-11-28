// Filename: cmd/web/main.go

package main

import (
	"log"
	"net/http"

	"github.com/kelseyaban/echo/internal/ws"
)

func handlerHome(w http.ResponseWriter, r *http.Request){
    w.Write([]byte("WebSockets!\n"))
}

func main() {
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir("./web")))
	mux.HandleFunc("/test", handlerHome)
	mux.HandleFunc("/ws", ws.HandleWebSocket)
	log.Print("Starting server on :4000")
	err := http.ListenAndServe(":4000", mux)
	log.Fatal(err)
}
