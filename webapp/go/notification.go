package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// 通知を SSE で送るかどうか。false にすると、これまでのポーリング（JSON と retry_after_ms）に戻る
const notificationSSE = true

// SSE で、未送信の通知があるかを DB に確認する間隔
const notificationCheckInterval = 200 * time.Millisecond

// streamNotifications は SSE の接続を保ち、通知を送り続ける。
// app サーバーが複数台でも動くよう、アプリ内で通知を受け渡さず、接続ごとに DB を定期的に確認する。
//   - hasUnsent: 未送信の状態があるかを、軽いクエリで確認する
//   - build: 通知を1件作る。未送信の状態があれば古いものを1件送信済みにして consumed=true を返す。ライドが無ければ nil
func streamNotifications[T any](
	w http.ResponseWriter,
	r *http.Request,
	hasUnsent func(context.Context) (bool, error),
	build func(context.Context) (data *T, consumed bool, err error),
) {
	ctx := r.Context()
	rc := http.NewResponseController(w)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	// nginx にレスポンスをためこませず、すぐにクライアントへ流させる
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return
	}

	send := func(data *T) error {
		buf, err := json.Marshal(data)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", buf); err != nil {
			return err
		}
		return rc.Flush()
	}

	ticker := time.NewTicker(notificationCheckInterval)
	defer ticker.Stop()

	// 接続直後は、未送信がなくても最新の状態を送る（仕様で必須）
	first := true
	for {
		// 未送信の状態がある間は、待たずに古い順に続けて送る
		for {
			if !first {
				ok, err := hasUnsent(ctx)
				if err != nil {
					logStreamError(err)
					return
				}
				if !ok {
					break
				}
			}
			data, consumed, err := build(ctx)
			if err != nil {
				logStreamError(err)
				return
			}
			if data != nil && (first || consumed) {
				if err := send(data); err != nil {
					return
				}
			}
			first = false
			if !consumed {
				break
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func logStreamError(err error) {
	if !errors.Is(err, context.Canceled) {
		slog.Error("notification stream", "err", err)
	}
}
