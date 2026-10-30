// Command forwardperf measures embeddings-forward overhead through a real CPA
// host against a local zero-latency upstream fixture. It reports latency added
// versus calling the fixture directly, throughput under concurrency, and host
// memory with large embedding and decision payloads. No live provider calls occur.
//
//	go run ./tests/forwardperf -host /tmp/cpa-hosts/linux_amd64/cpa-cgo -plugins /tmp/fwd-libs
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const frontendKey = "perf-frontend-key"

func main() {
	host := flag.String("host", "", "CPA host executable")
	plugins := flag.String("plugins", "", "directory containing embeddings-forward.so")
	requests := flag.Int("n", 2000, "requests per latency/throughput scenario")
	flag.Parse()
	if err := run(*host, *plugins, *requests); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func embeddingsBody(count, dims int) []byte {
	var b bytes.Buffer
	b.WriteString(`{"object":"list","data":[`)
	for i := 0; i < count; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"object":"embedding","index":%d,"embedding":[`, i)
		for d := 0; d < dims; d++ {
			if d > 0 {
				b.WriteByte(',')
			}
			b.WriteString(strconv.FormatFloat(float64((i*dims+d)%1000)/1000-0.5, 'f', 9, 64))
		}
		b.WriteString("]}")
	}
	fmt.Fprintf(&b, `],"model":"text-embedding-3-small","usage":{"prompt_tokens":%d,"total_tokens":%d}}`, count, count)
	return b.Bytes()
}

func fixture() (*http.Server, string, error) {
	small := embeddingsBody(1, 1536)
	batch := embeddingsBody(100, 1536)
	large := embeddingsBody(2048, 1536)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input json.RawMessage `json:"input"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		var inputs []string
		if json.Unmarshal(req.Input, &inputs) == nil {
			if len(inputs) == 100 {
				_, _ = w.Write(batch)
			} else {
				_, _ = w.Write(large)
			}
			return
		}
		_, _ = w.Write(small)
	})
	mux.HandleFunc("/alpha/decisions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Questions map[string]json.RawMessage `json:"questions"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		answers := make(map[string]any, len(req.Questions))
		for key := range req.Questions {
			answers[key] = map[string]any{"type": "noul", "noul": 0.5}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "jev", "answers": answers, "usage": map[string]int{"input_tokens": len(answers), "output_tokens": 1}})
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", err
	}
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	return server, "http://" + listener.Addr().String(), nil
}

func freePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func run(hostPath, pluginDir string, n int) error {
	upstream, base, err := fixture()
	if err != nil {
		return err
	}
	defer upstream.Close()
	root, err := os.MkdirTemp("", "forwardperf-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	libDir := filepath.Join(root, "plugins", runtime.GOOS, runtime.GOARCH)
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		return err
	}
	library, err := os.ReadFile(filepath.Join(pluginDir, "embeddings-forward.so"))
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(libDir, "embeddings-forward.so"), library, 0o755); err != nil {
		return err
	}
	port, err := freePort()
	if err != nil {
		return err
	}
	config := fmt.Sprintf(`host: "127.0.0.1"
port: %d
auth-dir: %q
api-keys: [%s]
remote-management: {disable-control-panel: true}
plugins:
  enabled: true
  dir: %q
  configs:
    embeddings-forward:
      enabled: true
      embeddings:
        enabled: true
        providers: [{name: fixture, base_url: %q, api_keys: [perf-key], models: [{name: text-embedding-3-small, alias: emb}]}]
      decisions:
        enabled: true
        providers: [{name: fixture, base_url: %q, api_keys: [perf-key], max_questions: 100, max_concurrency: 4}]
`, port, filepath.Join(root, "auths"), frontendKey, filepath.Join(root, "plugins"), base+"/v1", base)
	if err := os.MkdirAll(filepath.Join(root, "auths"), 0o755); err != nil {
		return err
	}
	configPath := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		return err
	}
	logFile, err := os.Create(filepath.Join(root, "host.log"))
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.Command(hostPath, "--config", configPath, "--local-model")
	cmd.Dir, cmd.Stdout, cmd.Stderr = root, logFile, logFile
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		_ = cmd.Process.Signal(os.Interrupt)
		_ = cmd.Wait()
	}()
	cpa := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 256}}
	smallReq := []byte(`{"model":"emb","input":"hello world"}`)
	directReq := []byte(`{"model":"text-embedding-3-small","input":"hello world"}`)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if status, _, _ := post(client, cpa+"/v1/embeddings", smallReq, true); status == 200 {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("host not ready")
		}
		time.Sleep(100 * time.Millisecond)
	}
	rss := func() string { return procStatus(cmd.Process.Pid) }
	fmt.Printf("host %s, GOMAXPROCS=%d, n=%d\nidle host memory: %s\n\n", filepath.Base(hostPath), runtime.GOMAXPROCS(0), n, rss())

	fmt.Println("| scenario | concurrency | direct p50 / p99 | via CPA p50 / p99 | added p50 / p99 | direct rps | CPA rps |")
	fmt.Println("|---|---|---|---|---|---|---|")
	for _, c := range []int{1, 16, 64} {
		direct := measure(client, base+"/v1/embeddings", directReq, false, n, c)
		via := measure(client, cpa+"/v1/embeddings", smallReq, true, n, c)
		report("embeddings 1x1536", c, direct, via)
	}
	questions := map[string]any{}
	for i := 0; i < 250; i++ {
		questions[fmt.Sprintf("q%03d", i)] = map[string]any{"type": "noul", "instructions": fmt.Sprintf("Is memory %d relevant to the query?", i)}
	}
	decision, _ := json.Marshal(map[string]any{"model": "jev", "state": "query", "questions": questions})
	for _, c := range []int{1, 16} {
		direct := measure(client, base+"/alpha/decisions", decision, false, n/4, c)
		via := measure(client, cpa+"/api/alpha/decisions", decision, true, n/4, c)
		report("decisions 250q (3 batches via CPA)", c, direct, via)
	}

	hindsight := make([]string, 100)
	for i := range hindsight {
		hindsight[i] = strings.Repeat("memory text ", 40) + strconv.Itoa(i)
	}
	batchReq, _ := json.Marshal(map[string]any{"model": "emb", "input": hindsight})
	directBatch, _ := json.Marshal(map[string]any{"model": "text-embedding-3-small", "input": hindsight})
	for _, c := range []int{1, 16} {
		direct := measure(client, base+"/v1/embeddings", directBatch, false, n/8, c)
		via := measure(client, cpa+"/v1/embeddings", batchReq, true, n/8, c)
		report(fmt.Sprintf("embeddings 100x1536 (%.1f MiB)", float64(len(embeddingsBody(100, 1536)))/(1<<20)), c, direct, via)
	}
	fmt.Printf("host memory after 100-input batches: %s\n", rss())

	inputs := make([]string, 2048)
	for i := range inputs {
		inputs[i] = strings.Repeat("token ", 32) + strconv.Itoa(i)
	}
	largeReq, _ := json.Marshal(map[string]any{"model": "emb", "input": inputs})
	directLarge, _ := json.Marshal(map[string]any{"model": "text-embedding-3-small", "input": inputs})
	fmt.Printf("\nlarge embedding request %.1f KiB, response %.1f MiB\n", float64(len(largeReq))/1024, float64(len(embeddingsBody(2048, 1536)))/(1<<20))
	for _, c := range []int{1, 4} {
		direct := measure(client, base+"/v1/embeddings", directLarge, false, 40, c)
		via := measure(client, cpa+"/v1/embeddings", largeReq, true, 40, c)
		report("embeddings 2048x1536", c, direct, via)
		fmt.Printf("host memory after concurrency %d: %s\n", c, rss())
	}
	bigQuestions := map[string]any{}
	for i := 0; i < 2000; i++ {
		bigQuestions[fmt.Sprintf("q%04d", i)] = map[string]any{"type": "noul", "instructions": strings.Repeat("Is this memory relevant? ", 20)}
	}
	bigDecision, _ := json.Marshal(map[string]any{"model": "jev", "state": strings.Repeat("state ", 2000), "questions": bigQuestions})
	via := measure(client, cpa+"/api/alpha/decisions", bigDecision, true, 40, 4)
	direct := measure(client, base+"/alpha/decisions", bigDecision, false, 40, 4)
	fmt.Printf("\nlarge decisions request %.1f KiB, 2000 questions (20 batches via CPA)\n", float64(len(bigDecision))/1024)
	report("decisions 2000q", 4, direct, via)
	fmt.Printf("host memory after large decisions: %s\n", rss())
	return nil
}

type result struct {
	latencies []time.Duration
	elapsed   time.Duration
	failures  int
}

func measure(client *http.Client, url string, body []byte, auth bool, n, concurrency int) result {
	for i := 0; i < min(n, 20); i++ {
		_, _, _ = post(client, url, body, auth)
	}
	latencies := make([]time.Duration, n)
	failures := 0
	var mu sync.Mutex
	jobs := make(chan int)
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				status, elapsed, _ := post(client, url, body, auth)
				latencies[i] = elapsed
				if status != 200 {
					mu.Lock()
					failures++
					mu.Unlock()
				}
			}
		}()
	}
	for i := 0; i < n; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return result{latencies: latencies, elapsed: time.Since(start), failures: failures}
}

func post(client *http.Client, url string, body []byte, auth bool) (int, time.Duration, error) {
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if auth {
		req.Header.Set("Authorization", "Bearer "+frontendKey)
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, time.Since(start), err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, time.Since(start), nil
}

func percentile(values []time.Duration, p float64) time.Duration {
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[min(len(sorted)-1, int(float64(len(sorted))*p))]
}

func ms(d time.Duration) string { return fmt.Sprintf("%.2f", float64(d.Microseconds())/1000) }

func report(name string, concurrency int, direct, via result) {
	d50, d99 := percentile(direct.latencies, 0.5), percentile(direct.latencies, 0.99)
	v50, v99 := percentile(via.latencies, 0.5), percentile(via.latencies, 0.99)
	failed := ""
	if direct.failures+via.failures > 0 {
		failed = fmt.Sprintf(" (failures direct=%d cpa=%d)", direct.failures, via.failures)
	}
	fmt.Printf("| %s%s | %d | %s / %s ms | %s / %s ms | %s / %s ms | %.0f | %.0f |\n", name, failed, concurrency,
		ms(d50), ms(d99), ms(v50), ms(v99), ms(v50-d50), ms(v99-d99),
		float64(len(direct.latencies))/direct.elapsed.Seconds(), float64(len(via.latencies))/via.elapsed.Seconds())
}

func procStatus(pid int) string {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return "unavailable"
	}
	var parts []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "VmRSS:") || strings.HasPrefix(line, "VmHWM:") {
			fields := strings.Fields(line)
			kb, _ := strconv.Atoi(fields[1])
			parts = append(parts, fmt.Sprintf("%s %.0f MiB", strings.TrimSuffix(fields[0], ":"), float64(kb)/1024))
		}
	}
	return strings.Join(parts, ", ")
}
