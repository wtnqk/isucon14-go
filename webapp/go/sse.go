package main

import (
	"net/http"
	"sync"
)

// sseContentType は SSE の Content-Type。
//
// この文字列はベンチマーカーの分岐条件そのものになっている。
// bench/benchmarker/webapp/client_user.go と client_chair.go は
//
//	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
//
// で SSE かどうかを判定する。ここが一致しないと、ベンチマーカーは通常の JSON レスポンスとして
// 扱い、retry_after_ms を待って叩き直す。こちらは何も返さないので、次の状態遷移まで通知が届かず、
// 「状態が変更されてから3秒以内に通知」の要件を静かに破ることになる。
// 大小文字も区別される（strings.Contains は大小文字を無視しない）ので、小文字で固定する。
const sseContentType = "text/event-stream; charset=utf-8"

// sseWriter は text/event-stream のレスポンスを書く。
// Flush しないとバッファに溜まって届かないので、1通知ごとに Flush する
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	mu      sync.Mutex
	err     error
	// wrote bool 一度でも書き込みを試みたかどうか
	wrote bool
}

func newSSEWriter(w http.ResponseWriter) (*sseWriter, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	header := w.Header()
	header.Set("Content-Type", sseContentType)
	header.Set("Cache-Control", "no-cache, no-transform")
	header.Set("Connection", "keep-alive")
	// nginx などのリバースプロキシにバッファさせない
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return &sseWriter{w: w, flusher: flusher}, true
}

// WriteData は通知を1つ送る。data は1行で送る必要があるので、改行が混ざっていたら取り除く
func (s *sseWriter) WriteData(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.wrote = true
	s.err = writeSSE(s.w, "data: ", data)
	return s.err
}

// Heartbeat は接続を維持するためのコメント行を送る。
// 通知は仕様上は1行の data: だけなので、コメント行は無視してよい
func (s *sseWriter) Heartbeat() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.wrote = true
	s.err = writeSSE(s.w, ":", []byte(" heartbeat"))
	if s.err == nil {
		s.flusher.Flush()
	}
	return s.err
}

func (s *sseWriter) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.flusher.Flush()
	return nil
}

// Wrote は、この接続で通知を1つでも送ったかどうかを返す。
// 1つも送らないまま終わるのは、状態遷移が無いまま接続が切れた場合だけなので、
// 呼び出し側はこれで「何も流せなかった」ことを判定できる
func (s *sseWriter) Wrote() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wrote
}

func writeSSE(w http.ResponseWriter, prefix string, data []byte) error {
	buf := make([]byte, 0, len(prefix)+len(data)+2)
	buf = append(buf, prefix...)
	// `data:` の直後は空白1つ。コメント行（prefix が ":"）は ": " の形にする
	buf = append(buf, ' ')
	for _, b := range data {
		// JSON に改行は入らないが、入っていても1行に収める
		if b == '\n' || b == '\r' {
			continue
		}
		buf = append(buf, b)
	}
	buf = append(buf, '\n', '\n')
	_, err := w.Write(buf)
	return err
}
