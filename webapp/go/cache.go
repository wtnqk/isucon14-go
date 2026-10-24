package main

import (
	"context"
	"sync"
)

// 一度作られたら変わらないデータを、プロセス内に持って DB への問い合わせを減らす。
// users と owners は更新されない。chairs は is_active と updated_at だけが更新されるので、
// キャッシュから取った Chair のこの2つは古い可能性がある（使う側で参照しないこと）。
// キャッシュになければ DB から読むので、app サーバーが複数台でも、別の台で作られたデータを正しく取れる。

type cache[V any] struct {
	mu sync.RWMutex
	m  map[string]V
}

func newCache[V any]() *cache[V] {
	return &cache[V]{m: map[string]V{}}
}

func (c *cache[V]) get(key string) (V, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.m[key]
	return v, ok
}

func (c *cache[V]) set(key string, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = v
}

func (c *cache[V]) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m = map[string]V{}
}

var (
	userByToken          = newCache[User]()
	userByID             = newCache[User]()
	ownerByToken         = newCache[Owner]()
	ownerByRegisterToken = newCache[Owner]()
	chairByToken         = newCache[Chair]()
	chairByID            = newCache[Chair]()
)

// resetCaches は POST /api/initialize でデータが作り直されたときに呼ぶ
func resetCaches() {
	userByToken.reset()
	userByID.reset()
	ownerByToken.reset()
	ownerByRegisterToken.reset()
	chairByToken.reset()
	chairByID.reset()
}

// fetchCached はキャッシュにあればそれを、なければ DB から読んでキャッシュに入れて返す。
// 見つからなければ sql.ErrNoRows を返す（見つからなかったことはキャッシュしない）。
// 呼び出し側が書き換えても影響しないよう、コピーを返す
func fetchCached[V any](ctx context.Context, c *cache[V], key string, query string) (*V, error) {
	if v, ok := c.get(key); ok {
		return &v, nil
	}
	var v V
	if err := db.GetContext(ctx, &v, query, key); err != nil {
		return nil, err
	}
	c.set(key, v)
	return &v, nil
}

func getUserByToken(ctx context.Context, token string) (*User, error) {
	return fetchCached(ctx, userByToken, token, "SELECT * FROM users WHERE access_token = ?")
}

func getUserByID(ctx context.Context, id string) (*User, error) {
	return fetchCached(ctx, userByID, id, "SELECT * FROM users WHERE id = ?")
}

func getOwnerByToken(ctx context.Context, token string) (*Owner, error) {
	return fetchCached(ctx, ownerByToken, token, "SELECT * FROM owners WHERE access_token = ?")
}

func getOwnerByRegisterToken(ctx context.Context, token string) (*Owner, error) {
	return fetchCached(ctx, ownerByRegisterToken, token, "SELECT * FROM owners WHERE chair_register_token = ?")
}

func getChairByToken(ctx context.Context, token string) (*Chair, error) {
	return fetchCached(ctx, chairByToken, token, "SELECT * FROM chairs WHERE access_token = ?")
}

func getChairByID(ctx context.Context, id string) (*Chair, error) {
	return fetchCached(ctx, chairByID, id, "SELECT * FROM chairs WHERE id = ?")
}
