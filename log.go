package work

import "fmt"

func logError(key string, err error) {
	fmt.Printf("ERROR: %s - %s\n", key, err.Error())
}

func logInfo(key string, msg string) {
	fmt.Printf("INFO: %s - %s\n", key, msg)
}
