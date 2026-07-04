package main
import (
  "fmt"
  "log"
  "os"
  "home-datacenter-api/internal/database"
  "home-datacenter-api/internal/repository"
  "home-datacenter-api/internal/service"
)
func main() {
  database.InitDB("D:/Projects/home-datacenter/data/sqlite/app.db")
  dr := repository.NewDeviceRepository(database.DB)
  ds := service.NewDeviceService(dr)
  name := "debug-device"
  if len(os.Args) > 1 { name = os.Args[1] }
  ak, err := ds.CreateDevice(1, name)
  if err != nil { log.Fatal(err) }
  fmt.Println("ACCESS_KEY=" + ak)
}
