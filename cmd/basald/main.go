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
		addr     = flag.String("addr", "127.0.0.1:8080", "HTTP listen address (use 0.0.0.0:8080 to bind every interface)")
		file     = flag.String("file", "basal.bsl", "path of the .bsl container")
		layers   = flag.String("layers", "Accounts,Teams,Users", "layer definitions used when creating a new container; optional attribute names enable symbolic BQL (\"0:Accounts(name,date),1:Teams(name)\" or \"Accounts,Teams\")")
		author   = flag.String("author", "basald <basald@localhost>", "Author of the initialization commit")
		summary  = flag.String("summary", "Database initialized", "Summary of the initialization commit")
		reason   = flag.String("reason", "daemon startup on empty container", "Reason of the initialization commit")
		syncF    = flag.Bool("sync", true, "fsync every commit before acknowledging it (pass -sync=false to skip)")
		tokenEnv = flag.String("token-env", "BASAL_TOKEN", "environment variable holding the bearer token (never pass the token as an argument)")
		readEnv  = flag.String("read-token-env", "", "environment variable holding a read-only bearer token")
		insecure = flag.Bool("insecure-no-auth", false, "allow a non-loopback listen with no token")
		tlsCert  = flag.String("tls-cert", "", "TLS certificate file; requires -tls-key")
		tlsKey   = flag.String("tls-key", "", "TLS private key file; requires -tls-cert")
		maxMem   = flag.String("max-memory", "", "page cache budget, for example 256MB (default 64MB). The id index is mapped; secondary indexes stay resident")
	)
	flag.Parse()

	addrSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "addr" {
			addrSet = true
		}
	})
	// Cloud Run (and similar platforms) inject PORT. Bind every interface
	// only in that case; a plain `basald` stays on localhost.
	if !addrSet && os.Getenv("PORT") != "" {
		*addr = "0.0.0.0:" + os.Getenv("PORT")
	}

	token := os.Getenv(*tokenEnv)
	readToken := ""
	if *readEnv != "" {
		readToken = os.Getenv(*readEnv)
		if readToken == "" {
			log.Fatalf("basald: -read-token-env %s is unset or empty", *readEnv)
		}
	}
	gate := token
	if gate == "" {
		gate = readToken
	}
	if err := api.ValidateListen(*addr, gate, *insecure); err != nil {
		log.Fatalf("basald: %v", err)
	}
	if (*tlsCert == "") != (*tlsKey == "") {
		log.Fatal("basald: -tls-cert and -tls-key must be set together")
	}
	if !api.Loopback(*addr) && token == "" && *insecure {
		log.Printf("basald: warning: listening on %s with no authentication (-insecure-no-auth)", *addr)
	}
	if !api.Loopback(*addr) && *tlsCert == "" {
		log.Printf("basald: %s is not loopback and TLS is off; terminate TLS in front of basald or pass -tls-cert and -tls-key", *addr)
	}

	db, err := openOrCreate(*file, *layers, engine.TxMeta{Author: *author, Summary: *summary, Reason: *reason})
	if err != nil {
		log.Fatalf("basald: %v", err)
	}
	db.SetSync(*syncF)
	if *maxMem != "" {
		n, err := engine.ParseByteSize(*maxMem)
		if err != nil {
			log.Fatalf("basald: %v", err)
		}
		db.SetCacheLimit(int(n))
	}

	server := api.NewServer(db)
	server.SetAuth(token, readToken)
	srv := &http.Server{
		Addr:              *addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	go func() {
		log.Printf("basald: serving %s on %s (branch %s, sync %v)", db.Path(), *addr, engine.DefaultBranch, *syncF)
		var err error
		if *tlsCert != "" {
			err = srv.ListenAndServeTLS(*tlsCert, *tlsKey)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
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
