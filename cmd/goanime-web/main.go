package main

import (
	"log"
	"os"

	"github.com/alvarorichard/Goanime/internal/util"
	"github.com/alvarorichard/Goanime/internal/web"
)

func main() {
	util.InitLogger()

	addr := os.Getenv("GOANIME_WEB_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	srv := web.NewServer()
	log.Printf("GoAnime Web running at http://localhost%s", addr)
	if err := srv.Start(addr); err != nil {
		log.Fatalf("failed to start GoAnime Web: %v", err)
	}
}
