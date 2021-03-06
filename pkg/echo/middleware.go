package echo

import (
	"fmt"
	"log"
	"net/http"
	"time"
)

func Recovery() Handler {
	return func(c Context) error {
		defer func() {
			if err := recover(); err != nil {
				message := fmt.Sprintf("%#v", err)
				log.Println(message)
				c.String(http.StatusInternalServerError, "Internal Server Error")
			}
		}()
		return c.Next()
	}
}

func Logger() Handler {
	return func(c Context) error {
		t := time.Now()
		err := c.Next()
		log.Printf("[%d] %s in %v", c.StatusCode(), c.Request().RequestURI, time.Since(t))
		return err
	}
}
