// Command basald is the BasalDB HTTP daemon: it opens a .bsl container and
// serves the REST API (BQL queries over MVCC snapshots, concurrent writes,
// branching). Reads and writes proceed at the same time.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"basaldb/pkg/api"
	"basaldb/pkg/engine"
)

func main() {
	var (
		addr    = flag.String("addr", ":8080", "HTTP listen address")
		file    = flag.String("file", "basal.bsl", "path of the .bsl container")
		layers  = flag.String("layers", "Accounts,Teams,Users", "layer definitions used when creating a new container; optional attribute names enable symbolic BQL (\"0:Accounts(name,date),1:Teams(name)\" or \"Accounts,Teams\")")
		author  = flag.String("author", "basald <basald@localhost>", "Author of the initialization commit")
		summary = flag.String("summary", "Database initialized", "Summary of the initialization commit")
		reason  = flag.String("reason", "daemon startup on empty container", "Reason of the initialization commit")
		syncF   = flag.Bool("sync", false, "fsync every commit (slower, safest)")
	)
	flag.Parse()

	// Cloud Run (and similar platforms) inject PORT instead of -addr.
	if *addr == ":8080" && os.Getenv("PORT") != "" {
		*addr = ":" + os.Getenv("PORT")
	}

	db, err := openOrCreate(*file, *layers, engine.TxMeta{Author: *author, Summary: *summary, Reason: *reason})
	if err != nil {
		log.Fatalf("basald: %v", err)
	}
	db.SetSync(*syncF)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           api.NewServer(db).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("basald: serving %s on %s (branch %s)", db.Path(), *addr, engine.DefaultBranch)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("basald: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Print("basald: shutting down (checkpointing)")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("basald: shutdown: %v", err)
	}
	if err := db.Close(); err != nil {
		log.Printf("basald: checkpoint: %v", err)
	}
}

func openOrCreate(path, layersFlag string, meta engine.TxMeta) (*engine.DB, error) {
	if _, err := os.Stat(path); err == nil {
		return engine.Open(path)
	}
	layers, err := parseLayers(layersFlag)
	if err != nil {
		return nil, err
	}
	log.Printf("basald: creating %s with layers %v", path, layers)
	return engine.Init(path, layers, meta)
}

// parseLayers accepts "Accounts,Teams,Users" (heights assigned in order),
// "0:Accounts,1:Teams,2:Users" (explicit heights), and optional
// attribute-name declarations "0:Accounts(name,date)" for symbolic BQL.
func parseLayers(flagValue string) ([]engine.Layer, error) {
	return engine.ParseLayers(flagValue)
}
