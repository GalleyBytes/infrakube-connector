package util

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
)

func EnvOrDefault(s, defaultval string) string {
	env := os.Getenv(s)
	if env == "" {
		return defaultval
	}
	return env
}

func EnvOrPanic(env string) string {
	value := os.Getenv(env)
	if value == "" {
		log.Panicf("%s is required", env)
	}
	return value
}

func Pprint(o interface{}) {
	b, err := json.Marshal(o)
	if err != nil {
		log.Panic(err)
	}
	var out bytes.Buffer
	err = json.Indent(&out, b, "", "  ")
	if err != nil {
		log.Panic(err)
	}
	fmt.Println(out.String())
}
