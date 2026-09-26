// Spike harness: scripted backend (:19101) + stub ext_proc (:19100).
// Backend behaviour per attempt is taken from header x-modes (comma list),
// indexed by how many times this x-spike-id has been seen.
package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extproc "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
)

var (
	mu     sync.Mutex
	counts = map[string]int{}
)

func backend(w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get("x-spike-id")
	if r.URL.Path == "/__count" {
		mu.Lock()
		fmt.Fprintf(w, "%d", counts[r.URL.Query().Get("id")])
		mu.Unlock()
		return
	}
	body, _ := io.ReadAll(r.Body)
	mu.Lock()
	n := counts[id]
	counts[id] = n + 1
	mu.Unlock()
	modes := strings.Split(r.Header.Get("x-modes"), ",")
	mode := modes[len(modes)-1]
	if n < len(modes) {
		mode = modes[n]
	}
	w.Header().Set("x-attempt", strconv.Itoa(n+1))
	w.Header().Set("x-extproc-seen", "first="+r.Header.Get("x-extproc-first")+",second="+r.Header.Get("x-extproc-second"))
	switch {
	case mode == "ok":
		w.WriteHeader(200)
		fmt.Fprintf(w, `{"ok":true,"attempt":%d,"bodylen":%d}`, n+1, len(body))
	case strings.HasPrefix(mode, "status:"):
		code, _ := strconv.Atoi(strings.TrimPrefix(mode, "status:"))
		w.WriteHeader(code)
		fmt.Fprintf(w, `{"status":%d}`, code)
	case strings.HasPrefix(mode, "hang:"):
		s, _ := strconv.Atoi(strings.TrimPrefix(mode, "hang:"))
		time.Sleep(time.Duration(s) * time.Second)
		w.WriteHeader(200)
		fmt.Fprint(w, `{"late":true}`)
	case strings.HasPrefix(mode, "stream:"):
		s, _ := strconv.Atoi(strings.TrimPrefix(mode, "stream:"))
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(200)
		for i := 0; i < s*2; i++ {
			fmt.Fprintf(w, "data: chunk%d\n\n", i)
			w.(http.Flusher).Flush()
			time.Sleep(500 * time.Millisecond)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

type extProcServer struct {
	extproc.UnimplementedExternalProcessorServer
}

func (extProcServer) Process(stream extproc.ExternalProcessor_ProcessServer) error {
	for {
		req, err := stream.Recv()
		if err != nil {
			return nil
		}
		resp := &extproc.ProcessingResponse{}
		switch req.Request.(type) {
		case *extproc.ProcessingRequest_RequestHeaders:
			key := "x-extproc-first"
			for _, h := range req.GetRequestHeaders().GetHeaders().GetHeaders() {
				if h.Key == "x-extproc-first" {
					key = "x-extproc-second"
				}
			}
			resp.Response = &extproc.ProcessingResponse_RequestHeaders{RequestHeaders: &extproc.HeadersResponse{
				Response: &extproc.CommonResponse{HeaderMutation: &extproc.HeaderMutation{
					SetHeaders: []*corev3.HeaderValueOption{{
						Header:       &corev3.HeaderValue{Key: key, RawValue: []byte("1")},
						AppendAction: corev3.HeaderValueOption_APPEND_IF_EXISTS_OR_ADD,
					}},
				}},
			}}
		case *extproc.ProcessingRequest_ResponseHeaders:
			resp.Response = &extproc.ProcessingResponse_ResponseHeaders{ResponseHeaders: &extproc.HeadersResponse{}}
		default:
			continue
		}
		if err := stream.Send(resp); err != nil {
			return nil
		}
	}
}

func main() {
	go func() {
		lis, err := net.Listen("tcp", "127.0.0.1:19100")
		if err != nil {
			log.Fatal(err)
		}
		s := grpc.NewServer()
		extproc.RegisterExternalProcessorServer(s, extProcServer{})
		log.Fatal(s.Serve(lis))
	}()
	log.Fatal(http.ListenAndServe("127.0.0.1:19101", http.HandlerFunc(backend)))
}
