BIN := claude-chatlog

.PHONY: build clean

build:
	@rm -f $(BIN)
	go build -o $(BIN) .

clean:
	@rm -f $(BIN)
