package redis

import (
	"context"
	"fmt"

	goredis "github.com/redis/go-redis/v9"
)

func NewClient(addr, password string, db int) (*goredis.Client, error) {
	client := goredis.NewClient(&goredis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})
	if err := client.Ping(context.Background()).Err(); err != nil {
		return nil, fmt.Errorf("redis: ping: %w", err)
	}
	return client, nil
}
