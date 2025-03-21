package main

import (
	// Go Internal Packages
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	// Local Packages
	config "tx-stream/config"
	kafka "tx-stream/kafka"
	mongodb "tx-stream/repositories/mongodb"
	redis "tx-stream/repositories/redis"
	txsvc "tx-stream/services/transactions"

	// External Packages
	"github.com/alecthomas/kingpin/v2"
	_ "github.com/jsternberg/zap-logfmt"
	"github.com/knadh/koanf"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/twmb/franz-go/plugin/kprom"
	"go.uber.org/zap"
)

// LoadSecrets Loads the secret variables and overrides the config
func LoadSecrets(k config.Config) config.Config {
	MongoURI := os.Getenv("MONGO_URI")
	if MongoURI != "" {
		k.Mongo.URI = MongoURI
	}

	RedisURI := os.Getenv("REDIS_URI")
	if RedisURI != "" {
		k.Redis.URI = RedisURI
	}

	RedisPWD := os.Getenv("REDIS_PWD")
	if RedisPWD != "" {
		k.Redis.Password = RedisPWD
	}

	KafkaBrokers := os.Getenv("KAFKA_BROKERS")
	if KafkaBrokers != "" {
		k.Kafka.Brokers = KafkaBrokers
	}

	IsProdMode := os.Getenv("IS_PROD_MODE")
	k.IsProdMode = IsProdMode == "true"
	return k
}

// LoadConfig loads the default configuration and overrides it with the config file
// specified by the path defined in the config flag
func LoadConfig() *koanf.Koanf {
	configPathMsg := "Path to the application config file"
	configPath := kingpin.Flag("config", configPathMsg).Short('c').Default("config.yml").String()

	kingpin.Parse()
	k := koanf.New(".")
	_ = k.Load(rawbytes.Provider(config.DefaultConfig), yaml.Parser())
	if *configPath != "" {
		_ = k.Load(file.Provider(*configPath), yaml.Parser())
	}
	return k
}

func main() {
	k := LoadConfig()

	// Unmarshalling config into struct
	appKonf := config.Config{}
	err := k.Unmarshal("", &appKonf)
	if err != nil {
		log.Fatalf("Error loading config: %v", err)
	}

	// Update and Validate config before starting the server
	prodKonf := LoadSecrets(appKonf)
	if err = prodKonf.Validate(); err != nil {
		log.Fatalf("Invalid configuration: %v", err)
	}

	if !prodKonf.IsProdMode {
		k.Print()
	}

	cfg := zap.NewProductionConfig()
	cfg.Encoding = "logfmt"
	_ = cfg.Level.UnmarshalText([]byte(k.String("logger.level")))
	cfg.InitialFields = make(map[string]any)
	cfg.InitialFields["host"], _ = os.Hostname()
	cfg.InitialFields["service"] = prodKonf.Application
	cfg.OutputPaths = []string{"stdout"}
	logger, _ := cfg.Build()
	defer func() {
		_ = logger.Sync()
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Mongo Connection
	mongoClient, err := mongodb.Connect(ctx, prodKonf.Mongo.URI)
	if err != nil {
		logger.Fatal("cannot create mongo client", zap.Error(err))
	}

	// Redis Connection
	redisClient, err := redis.Connect(ctx, prodKonf.Redis.URI, prodKonf.Redis.Password)
	if err != nil {
		logger.Fatal("cannot create redis client", zap.Error(err))
	}

	txRepo := mongodb.NewTxRepository(mongoClient)
	dlQueue := redis.NewDeadLetterQueue(redisClient, logger)
	txProcessor := txsvc.NewTxProcessor(logger, txRepo)

	metrics := kprom.NewMetrics("et")
	conf := &kafka.ConsumerConfig{
		Brokers:        []string{prodKonf.Kafka.Brokers},
		Name:           prodKonf.Kafka.ConsumerName,
		Topic:          prodKonf.Kafka.Topic,
		RecordsPerPoll: prodKonf.Kafka.RecordsPerPoll,
	}

	txConsumer, err := kafka.NewTxConsumer(conf, logger, txProcessor, dlQueue, metrics)
	if err != nil {
		logger.Fatal("cannot create transactions consumer", zap.Error(err))
	}

	err = txConsumer.Poll(ctx, prodKonf.Kafka.Consume)
	if err != nil {
		logger.Fatal("cannot poll records from topic", zap.Error(err))
	}
}
