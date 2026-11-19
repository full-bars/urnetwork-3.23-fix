package urnettools

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestExtenderCarrierAliasesLiveAndQueued(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket test")
	}
	for _, port := range []string{"443", "4053", "53"} {
		for _, sep := range []string{"-", "_"} {
			key := "extender" + sep + "udp" + port
			t.Run(key, func(t *testing.T) {
				for _, live := range []bool{false, true} {
					// a short path: the socket path limit is small on macOS and BSD
					dir, err := os.MkdirTemp("", "carrier-")
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = os.RemoveAll(dir) })
					p := Provider{StateDir: dir}
					var listener net.Listener
					requests := make(chan controlRequest, 2)
					if live {
						var err error
						listener, err = net.Listen("unix", filepath.Join(dir, "provider.sock"))
						if err != nil {
							t.Fatal(err)
						}
						defer listener.Close()
						go func() {
							for i := 0; i < 2; i++ {
								conn, err := listener.Accept()
								if err != nil {
									return
								}
								var req controlRequest
								if json.NewDecoder(conn).Decode(&req) == nil {
									requests <- req
									_ = json.NewEncoder(conn).Encode(controlResponse{OK: true})
								}
								conn.Close()
							}
						}()
					}
					for _, value := range []string{"on", "off", "invalid"} {
						err := applySetOverride(p, key, value, false)
						if (err != nil) != (value == "invalid") {
							t.Fatalf("live=%v key=%s value=%s err=%v", live, key, value, err)
						}
						if live && value != "invalid" {
							got := <-requests
							if got.Cmd != "set" || got.Key != "extender_udp"+port || got.Value != value {
								t.Fatalf("request: %+v", got)
							}
						}
					}
					if !live {
						data, err := os.ReadFile(filepath.Join(dir, "pending_overrides.json"))
						if err != nil {
							t.Fatal(err)
						}
						var ops []pendingOp
						if err := json.Unmarshal(data, &ops); err != nil {
							t.Fatal(err)
						}
						if len(ops) != 2 || ops[0] != (pendingOp{Op: "set", Key: "extender_udp" + port, Value: "on"}) || ops[1] != (pendingOp{Op: "set", Key: "extender_udp" + port, Value: "off"}) {
							t.Fatalf("queue: %+v", ops)
						}
					}
				}
			})
		}
	}
}
