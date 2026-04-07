.PHONY: build run backtest test clean lint

BINARY := bin/polysnipe
CONFIG  := config.yaml

build:
	go build -o $(BINARY) ./cmd/polysnipe

run: build
	./$(BINARY) -config $(CONFIG)

backtest: build
	@echo "Running backtest (set backtest.enabled=true in config.yaml first)..."
	./$(BINARY) -config $(CONFIG)

test:
	go test ./...

lint:
	go vet ./...
	@which golangci-lint > /dev/null 2>&1 && golangci-lint run ./... || echo "golangci-lint not installed; skipping"

clean:
	rm -rf bin/ data/backtest_results/ logs/

# Quick backtest: temporarily enables backtest mode and runs.
backtest-quick:
	@cp $(CONFIG) $(CONFIG).bak
	@sed -i 's/^  enabled: false.*# set true to run in backtest mode/  enabled: true/' $(CONFIG)
	./$(BINARY) -config $(CONFIG) || true
	@cp $(CONFIG).bak $(CONFIG)
	@rm $(CONFIG).bak
