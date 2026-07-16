package tool

import (
	"errors"
	"io"
	"os"
)

func readPhase9File(path string, max int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errors.New("tool: workspace file exceeds byte limit")
	}
	return data, nil
}
