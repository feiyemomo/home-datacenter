package main

import (
	"fmt"
	"os"

	"home-datacenter-api/internal/utils"
)

func main() {
	utils.JWTSecret = os.Args[1]
	devID := 3
	if len(os.Args) > 2 {
		fmt.Sscanf(os.Args[2], "%d", &devID)
	}
	tok, err := utils.GenerateToken(1, uint(devID), 1)
	if err != nil {
		fmt.Println("err:", err)
		os.Exit(1)
	}
	fmt.Print(tok)
}