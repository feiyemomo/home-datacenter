package main

import (
	"fmt"
	"log"
	"path/filepath"

	"home-datacenter-api/internal/database"
	"home-datacenter-api/internal/repository"
	"home-datacenter-api/internal/service"
)

func main() {

	// 初始化数据库
	// Resolve path relative to the project root, not the script's
	// CWD. The script lives at services/api/scripts/ — three levels
	// up reaches the project root, where the docker volume mount
	// ./data/sqlite lives. Using filepath.Abs ensures the path
	// works regardless of where `go run` is invoked from.
	dbPath, err := filepath.Abs("../../../data/sqlite/app.db")
	if err != nil {
		log.Fatalf("resolve db path: %v", err)
	}
	database.InitDB(dbPath)

	// 创建设备仓库
	deviceRepo := repository.NewDeviceRepository(
		database.DB,
	)

	// 创建设备服务
	deviceService := service.NewDeviceService(
		deviceRepo,
	)

	// ==========
	// 修改这里
	// ==========

	userID := uint(1)

	deviceName := "MacBook-Pro"

	// 创建设备
	accessKey, err := deviceService.CreateDevice(
		userID,
		deviceName,
	)

	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("===================================")
	fmt.Println("Device Created Successfully")
	fmt.Println("===================================")
	fmt.Printf("User ID     : %d\n", userID)
	fmt.Printf("Device Name : %s\n", deviceName)
	fmt.Println()
	fmt.Println("Access Key:")
	fmt.Println(accessKey)
	fmt.Println()
	fmt.Println("请立即保存该密钥，数据库不会保存明文。")
	fmt.Println("===================================")
}
