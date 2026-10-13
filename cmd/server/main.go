package main

import (
	"github.com/AdarshGT33/onvice/internal/cli"
	"github.com/gin-gonic/gin"
)

func main() {
	r := gin.Default()
	r.POST("/repo", cli.TakingInput)
	r.Run()
}
