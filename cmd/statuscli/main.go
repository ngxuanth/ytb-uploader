// CLI nhỏ để tra cứu tiến trình bất cứ lúc nào, không cần đọc log thủ công.
//
// Cách dùng:
//
//	statuscli list             -> liệt kê 20 job gần nhất
//	statuscli list 100         -> liệt kê 100 job gần nhất
//	statuscli get <job_id>     -> xem chi tiết 1 job
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"yt-uploader/internal/config"
	"yt-uploader/internal/store"
)

func main() {
	cfg := config.Load()

	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
	})
	defer rdb.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	st := store.New(rdb)

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "list":
		limit := int64(20)
		if len(os.Args) >= 3 {
			if n, err := strconv.ParseInt(os.Args[2], 10, 64); err == nil {
				limit = n
			}
		}
		listJobs(ctx, st, limit)

	case "get":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "thieu job_id. Vi du: statuscli get 1234567890-42")
			os.Exit(1)
		}
		getJob(ctx, st, os.Args[2])

	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`Cach dung:
  statuscli list             liet ke 20 job gan nhat
  statuscli list <n>         liet ke n job gan nhat
  statuscli get <job_id>     xem chi tiet 1 job (JSON day du)`)
}

func listJobs(ctx context.Context, st *store.Store, limit int64) {
	jobs, err := st.List(ctx, limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loi doc danh sach job: %v\n", err)
		os.Exit(1)
	}
	if len(jobs) == 0 {
		fmt.Println("chua co job nao.")
		return
	}

	fmt.Printf("%-22s %-11s %-4s %-16s %-20s %s\n", "JOB_ID", "STATE", "ATT.", "PROFILE", "ENQUEUED_AT", "FILE")
	for _, j := range jobs {
		profile := j.Profile
		if profile == "" {
			profile = "-"
		}
		fmt.Printf("%-22s %-11s %-4d %-16s %-20s %s\n",
			j.ID, j.State, j.Attempt, profile, j.EnqueuedAt.Local().Format("2006-01-02 15:04:05"), j.FilePath)
		if j.State == store.StateError || j.State == store.StateFailed {
			fmt.Printf("   -> loi: %s\n", j.Error)
		}
		if j.State == store.StateDone {
			fmt.Printf("   -> video: %s\n", j.VideoURL)
		}
	}
}

func getJob(ctx context.Context, st *store.Store, id string) {
	job, err := st.Get(ctx, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "khong tim thay job %s: %v\n", id, err)
		os.Exit(1)
	}
	out, _ := json.MarshalIndent(job, "", "  ")
	fmt.Println(string(out))
}
