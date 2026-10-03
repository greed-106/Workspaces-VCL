//go:build ignore

// 一次性工具:给 GPU 指标库造/清测试数据,用来验证服务端的降采样。
// 用法: go run seedtool.go <pg-url> seed|count|clean
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
)

func main() {
	ctx := context.Background()
	c, err := pgx.Connect(ctx, os.Args[1])
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer c.Close(ctx)
	switch os.Args[2] {
	case "seed":
		tag, err := c.Exec(ctx, `
			insert into gpu_samples (sampled_at, gpu_uuid, host_index, workspace_id, workspace_name, username,
				util_pct, mem_used_mib, mem_total_mib, power_w, temp_c)
			select now() - (i || ' minutes')::interval, 'GPU-reduce-test', 0, 'ws-reduce-test', 'reduce-test', 'tester',
			       (random()*100)::int, (random()*8000)::int, 12288, 200, 40
			from generate_series(1, 10080) i`)
		if err != nil {
			fmt.Println("seed:", err)
			os.Exit(1)
		}
		fmt.Println("  插入行数:", tag.RowsAffected())
	case "count":
		var n int
		_ = c.QueryRow(ctx, `select count(*) from gpu_samples where workspace_id='ws-reduce-test'`).Scan(&n)
		fmt.Println("  测试行数:", n)
	case "clean":
		tag, err := c.Exec(ctx, `delete from gpu_samples where workspace_id='ws-reduce-test'`)
		if err != nil {
			fmt.Println("clean:", err)
			os.Exit(1)
		}
		fmt.Println("  已清理行数:", tag.RowsAffected())
	}
}
