// main.go qoderwork2api 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"qoderwork2api/internal/cred"
	"qoderwork2api/internal/pool"
	"qoderwork2api/internal/scheduler"
	"qoderwork2api/internal/server"
	"qoderwork2api/internal/upstream"
	"qoderwork2api/internal/user"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("config %s not found, using defaults+env", *cfgPath)
			cfg, err = Load("")
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	absAuthDir, _ := filepath.Abs(cfg.AuthDir)
	absStateDir, _ := filepath.Abs(filepath.Dir(cfg.StateFile))

	// 加载 OAuth 凭证
	creds, err := cred.LoadDir(absAuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d auth(s) from %s", len(creds), absAuthDir)

	// 创建全局/管理员池
	adminPool := pool.New(cfg.StateFile)
	for _, c := range creds {
		c.EnsureMachineFingerprint()
		adminPool.Add(c)
	}
	adminPool.SaveState()

	// 创建上游客户端
	up := upstream.New()
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second

	// 创建调度器（管理全局池）
	sch := scheduler.New(scheduler.Config{
		Pool:           adminPool,
		Upstream:       up,
		CheckinHours:   cfg.Schedule.CheckinHours,
		KeepaliveHours: cfg.Schedule.KeepaliveHours,
	})

	// 加载用户存储
	userStorePath := filepath.Join(absStateDir, "users.json")
	userStore, err := user.NewStore(userStorePath)
	if err != nil {
		log.Fatalf("load users: %v", err)
	}

	// 创建用户池管理器
	poolMgr := user.NewPoolManager(absStateDir, up)

	// 为已有用户注册池（首次启动时为空，后续重启时加载）
	for _, u := range userStore.List() {
		poolMgr.RegisterUser(u.ID)
		log.Printf("registered user pool: %s (%s)", u.Name, u.ID)
	}

	// onReload: 重新扫描管理员 auth 目录
	onReload := func() {
		log.Println("reloading admin accounts...")
		newCreds, err := cred.LoadDir(absAuthDir)
		if err != nil {
			log.Printf("reload: load auths failed: %v", err)
			return
		}
		for _, c := range newCreds {
			c.EnsureMachineFingerprint()
		}
		adminPool.SyncToDir(newCreds)
		adminPool.SaveState()
		log.Printf("reload complete: %d account(s)", len(newCreds))
	}

	// 创建 HTTP handler
	h := server.NewHandler(server.Config{
		Pool:         adminPool,
		Upstream:     up,
		APIKey:       cfg.APIKey,
		HardCooldown: cfg.HardCreditDur,
		SoftCooldown: cfg.SoftRateDur,
		ErrThreshold: cfg.Cooldown.ErrThresh,
		ErrCooldown:  cfg.ErrCooldownDur,
		AuthDir:      absAuthDir,
		OnReload:     onReload,
		UserStore:    userStore,
		PoolMgr:      poolMgr,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("qoderwork2api listening on %s (api_key=%v, models=dynamic)", cfg.Listen, cfg.APIKey != "")
	log.Printf("admin UI: http://localhost%s/admin", cfg.Listen)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}
