package main

import (
	"fmt"
	"log"
	"os"
	"strconv"

	"home-datacenter-api/internal/database"
	"home-datacenter-api/internal/repository"
	"home-datacenter-api/internal/service"
)

func main() {
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		if _, err := os.Stat("/data/sqlite/app.db"); err == nil {
			dbPath = "/data/sqlite/app.db"
		} else if _, err := os.Stat("data/sqlite/app.db"); err == nil {
			dbPath = "data/sqlite/app.db"
		} else {
			dbPath = "./data/sqlite/app.db"
		}
	}

	deviceName := "tester_agent"
	if len(os.Args) > 1 {
		deviceName = os.Args[1]
	} else if envDev := os.Getenv("DEVICE_NAME"); envDev != "" {
		deviceName = envDev
	}

	userID := uint(1)
	if envUID := os.Getenv("USER_ID"); envUID != "" {
		if u, err := strconv.ParseUint(envUID, 10, 32); err == nil {
			userID = uint(u)
		}
	}

	database.InitDB(dbPath)
	dr := repository.NewDeviceRepository(database.DB)
	ds := service.NewDeviceService(dr)

	dev, ak, err := ds.CreateDevice(userID, deviceName, "Generated for testing agent")
	if err != nil {
		log.Fatalf("failed to create device: %v", err)
	}

	fmt.Printf("DEVICE_ID=%d\nDEVICE_NAME=%s\nACCESS_KEY=%s\n", dev.ID, dev.DeviceName, ak)
}
