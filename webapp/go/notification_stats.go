package main

import (
	"net/http"
	"runtime"
)

// notificationStatsResponse は通知まわりの今の状態
type notificationStatsResponse struct {
	// Subscribers 同時に張られている通知ストリームの数（ライド中のユーザーと椅子の合計）
	Subscribers int64 `json:"subscribers"`
	// Topics 購読者を持つトピックの数。購読者がいなくなったトピックは消えるので、
	// Subscribers に対するおおよその倍率になる
	Topics int64 `json:"topics"`
	// Publishes これまでに購読者を起こした回数
	Publishes int64 `json:"publishes"`
	// Dropped すでに起こし待ちだったため、起こすのを省いた回数
	Dropped int64 `json:"dropped"`
	// Goroutines 実行中の goroutine の数
	Goroutines int `json:"goroutines"`
	// OpenConnections DB 接続プールの現在の使用数
	OpenConnections int `json:"open_connections"`
	// InUseConnections DB 接続プールのうち使用中の数
	InUseConnections int `json:"in_use_connections"`
	// HeapAllocBytes 確保中のヒープのバイト数
	HeapAllocBytes uint64 `json:"heap_alloc_bytes"`
}

// internalGetNotificationStats は通知ストリームの接続数とメモリを返す。
// SSE はライド中ずっと接続を保持するので、同時ライド数が増えたときに
// どれだけリソースを使っているかを計測できるようにしておく
func internalGetNotificationStats(w http.ResponseWriter, r *http.Request) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	stats := db.Stats()

	hub.mu.Lock()
	topics := int64(len(hub.topics))
	hub.mu.Unlock()

	writeJSON(w, http.StatusOK, &notificationStatsResponse{
		Subscribers:      hub.subscribers.Load(),
		Topics:           topics,
		Publishes:        hub.publishes.Load(),
		Dropped:          hub.dropped.Load(),
		Goroutines:       runtime.NumGoroutine(),
		OpenConnections:  stats.OpenConnections,
		InUseConnections: stats.InUse,
		HeapAllocBytes:   m.HeapAlloc,
	})
}
