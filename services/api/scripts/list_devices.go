package main

import (
	"fmt"
	"log"

	"home-datacenter-api/internal/database"
	"home-datacenter-api/internal/model"
)

func main() {
	database.InitDB("../../data/sqlite/app.db")
	var devices []model.Device
	if err := database.DB.Find(&devices).Error; err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Found %d devices:\n", len(devices))
	for _, d := range devices {
		fmt.Printf("  ID=%d UserID=%d Name=%q Hash=%s Revoked=%v\n",
			d.ID, d.UserID, d.DeviceName, d.AccessKeyHash[:16], d.RevokedAt.Valid)
	}
}
