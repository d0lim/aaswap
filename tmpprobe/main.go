package main

import (
	json "encoding/json/v2"
	"encoding/json/jsontext"
	"fmt"
)

type Account struct {
	Email string                    `json:"email"`
	Alias string                    `json:"alias,omitzero"`
	Extra map[string]jsontext.Value `json:",embed"`
}

func main() {
	in := `{"email":"a@b.c","futureField":{"x":1},"alias":"work"}`
	var a Account
	if err := json.Unmarshal([]byte(in), &a); err != nil {
		fmt.Println("unmarshal err:", err)
		return
	}
	fmt.Printf("%+v\n", a)
	out, err := json.Marshal(a, jsontext.WithIndent("  "), json.Deterministic(true))
	fmt.Println(string(out), err)
}
