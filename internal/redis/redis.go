package redis

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

func InitRedis(ctx context.Context, dsn string) (*redis.Client, error) {
	appCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	client := redis.NewClient(
		&redis.Options{
			Addr: dsn,
		},
	)

	if err := client.Ping(appCtx).Err(); err != nil {
		return nil, err
	}

	return client, nil
}
