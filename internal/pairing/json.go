package pairing

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"
)

func uniqueJSON(data []byte) error {
	if !utf8.Valid(data) {
		return errors.New("invalid paired JSON encoding")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var value func(int) error
	value = func(depth int) error {
		if depth > 64 {
			return errors.New("paired JSON nesting exceeds limit")
		}
		token, err := dec.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				key, err := dec.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errors.New("duplicate paired JSON field")
				}
				seen[name] = true
				if err = value(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for dec.More() {
				if err = value(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid paired JSON")
		}
		_, err = dec.Token()
		return err
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing paired JSON")
	}
	return nil
}

// payloadJSON requires exact whitelist names and explicit required values.
// encoding/json alone accepts case aliases and null scalar zero values.
func payloadJSON(data []byte) error {
	if err := uniqueJSON(data); err != nil {
		return err
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	return exactJSON(value, reflect.TypeFor[Payload]())
}

func exactJSON(value any, typ reflect.Type) error {
	if value == nil {
		return errors.New("null paired setting")
	}
	if typ == reflect.TypeFor[time.Time]() {
		return nil // The typed decoder validates the timestamp.
	}
	if typ.Kind() == reflect.Struct {
		return exactObject(value, typ)
	}
	if typ.Kind() == reflect.Slice {
		items, ok := value.([]any)
		if !ok {
			return errors.New("invalid paired array")
		}
		for _, item := range items {
			if err := exactJSON(item, typ.Elem()); err != nil {
				return err
			}
		}
	}
	if typ.Kind() == reflect.Map {
		items, ok := value.(map[string]any)
		if !ok {
			return errors.New("invalid paired map")
		}
		for _, item := range items {
			if err := exactJSON(item, typ.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}

func exactObject(value any, typ reflect.Type) error {
	object, ok := value.(map[string]any)
	if !ok {
		return errors.New("invalid paired object")
	}
	fields := make(map[string]reflect.Type, typ.NumField())
	for i := range typ.NumField() {
		field := typ.Field(i)
		name, options, _ := strings.Cut(field.Tag.Get("json"), ",")
		fields[name] = field.Type
		if _, have := object[name]; !have && options == "" {
			return errors.New("missing paired setting")
		}
	}
	for name, item := range object {
		field, have := fields[name]
		if !have {
			return errors.New("unknown paired setting")
		}
		if err := exactJSON(item, field); err != nil {
			return err
		}
	}
	return nil
}
