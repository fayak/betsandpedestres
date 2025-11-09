package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"betsandpedestres/internal/auth"
	"betsandpedestres/internal/config"
	"betsandpedestres/internal/db"

	"github.com/jackc/pgconn"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/term"
)

func main() {
	log.SetFlags(0)

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "user":
		userCmd(os.Args[2:])
	case "gift":
		giftCmd(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println(`bap - betsandpedestres admin CLI

Usage:
  bap user create <username> [-display "<name>"] [-role user|moderator|admin] [-config config.yaml] [-db postgres://...]
  bap gift user <username> <amount> [-note "text"] [-config config.yaml] [-db postgres://...]
  bap gift all <amount>             [-note "text"] [-config config.yaml] [-db postgres://...]

Examples:
  bap user create alice
  bap user create bob -display "Bob Builder" -role moderator -config ./config.yaml
  bap gift user alice 100 -note "welcome bonus"
  bap gift all 25 -note "launch airdrop"`)
}

func userCmd(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "create":
		userCreate(args[1:])
	default:
		usage()
		os.Exit(2)
	}
}

func userCreate(args []string) {
	// Flags
	fs := flag.NewFlagSet("user create", flag.ExitOnError)
	fs.Init("user create", flag.ExitOnError)
	var (
		cfgPath     = fs.String("config", "config.yaml", "path to config file")
		dbOverride  = fs.String("db", "", "override database connection URL")
		displayName = fs.String("display", "", "display name (default: username)")
		role        = fs.String("role", "user", "role: unverified|user|moderator|admin")
	)
	_ = fs.Parse(reorderArgs(args))

	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Println("missing <username>")
		fmt.Println()
		usage()
		os.Exit(2)
	}
	username := strings.TrimSpace(rest[0])
	if username == "" {
		fmt.Println("username cannot be empty")
		os.Exit(2)
	}
	if *displayName == "" {
		*displayName = username
	}
	switch *role {
	case "unverified", "user", "moderator", "admin":
	default:
		fmt.Println("invalid role; must be one of: unverified|user|moderator|admin")
		os.Exit(2)
	}

	// Load config
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	// set JWT secret to ensure auth helpers are ready if you reuse them later
	auth.SetSecret(cfg.Security.JWTSecret)

	// DB pool
	appURL, err := resolveDBURL(cfg, *dbOverride)
	if err != nil {
		log.Fatalf("db url: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := db.NewPool(ctx, appURL)
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}
	defer pool.Close()

	// Prompt for password (twice)
	pw := promptPassword("Password: ")
	pw2 := promptPassword("Confirm password: ")
	if pw != pw2 {
		fmt.Println("passwords do not match")
		os.Exit(1)
	}
	if len(pw) < 6 {
		fmt.Println("password too short (min 6 chars for now)")
		os.Exit(1)
	}

	hash, err := auth.HashPassword(pw)
	if err != nil {
		log.Fatalf("hash password: %v", err)
	}

	// Insert user
	u, err := createUser(ctx, pool, username, *displayName, *role, hash)
	if err != nil {
		log.Fatalf("create user: %v", err)
	}
	fmt.Printf("ok: user created\n  id: %s\n  username: %s\n  role: %s\n", u.ID, u.Username, u.Role)
}

func promptPassword(prompt string) string {
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Fprintln(os.Stderr) // newline after input
	if err != nil {
		log.Fatalf("read password: %v", err)
	}
	return strings.TrimSpace(string(b))
}

type createdUser struct {
	ID          string
	Username    string
	DisplayName string
	Role        string
}

func createUser(ctx context.Context, pool *pgxpool.Pool, username, displayName, role, passwordHash string) (createdUser, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var u createdUser
	err := pool.QueryRow(ctx, `
		insert into users (username, display_name, password_hash, role)
		values ($1, $2, $3, $4)
		returning id, username, display_name, role
	`, username, displayName, passwordHash, role).Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role)

	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return u, fmt.Errorf("username %q already exists", username)
		}
		return u, err
	}
	return u, nil
}

func giftCmd(args []string) {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "user":
		giftUserCmd(args[1:])
	case "all":
		giftAllCmd(args[1:])
	default:
		usage()
		os.Exit(2)
	}
}

func giftUserCmd(args []string) {
	fs := flag.NewFlagSet("gift user", flag.ExitOnError)
	fs.Init("gift user", flag.ExitOnError)
	var (
		cfgPath    = fs.String("config", "config.yaml", "path to config file")
		dbOverride = fs.String("db", "", "override database connection URL")
		note       = fs.String("note", "", "optional note for the transaction")
	)
	_ = fs.Parse(reorderArgs(args))

	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Println("usage: bap gift user <username> <amount> [-note \"...\"] [-config config.yaml]")
		os.Exit(2)
	}
	username := strings.TrimSpace(rest[0])
	amount, err := strconv.ParseInt(rest[1], 10, 64)
	if err != nil || amount <= 0 {
		fmt.Println("amount must be a positive integer")
		os.Exit(2)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	auth.SetSecret(cfg.Security.JWTSecret)

	appURL, err := resolveDBURL(cfg, *dbOverride)
	if err != nil {
		log.Fatalf("db url: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := db.NewPool(ctx, appURL)
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}
	defer pool.Close()

	if err := giftToSingleUser(ctx, pool, username, amount, *note); err != nil {
		log.Fatalf("gift user: %v", err)
	}
	fmt.Printf("ok: gifted %d PiedPièce(s) to %s\n", amount, username)
}

func giftAllCmd(args []string) {
	fs := flag.NewFlagSet("gift all", flag.ExitOnError)
	fs.Init("gift all", flag.ExitOnError)
	var (
		cfgPath    = fs.String("config", "config.yaml", "path to config file")
		dbOverride = fs.String("db", "", "override database connection URL")
		note       = fs.String("note", "", "optional note for the transaction")
	)
	_ = fs.Parse(reorderArgs(args))

	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Println("usage: bap gift all <amount> [-note \"...\"] [-config config.yaml]")
		os.Exit(2)
	}
	amount, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil || amount <= 0 {
		fmt.Println("amount must be a positive integer")
		os.Exit(2)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	auth.SetSecret(cfg.Security.JWTSecret)

	appURL, err := resolveDBURL(cfg, *dbOverride)
	if err != nil {
		log.Fatalf("db url: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := db.NewPool(ctx, appURL)
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}
	defer pool.Close()

	n, err := giftToAllUsers(ctx, pool, amount, *note)
	if err != nil {
		log.Fatalf("gift all: %v", err)
	}
	fmt.Printf("ok: gifted %d PiedPièce(s) to each of %d user(s)\n", amount, n)
}

const houseUsername = "house"

func giftToSingleUser(ctx context.Context, pool *pgxpool.Pool, username string, amount int64, note string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Ensure house user and get its default account
	houseAccID, err := ensureHouseAccount(ctx, tx)
	if err != nil {
		return fmt.Errorf("house account: %w", err)
	}

	// Get recipient default account
	var targetUserID, targetAccID string
	err = tx.QueryRow(ctx, `
		select u.id, a.id
		from users u
		join accounts a on a.user_id = u.id and a.is_default
		where u.username = $1
	`, username).Scan(&targetUserID, &targetAccID)
	if err != nil {
		if err == pgx.ErrNoRows {
			return fmt.Errorf("user %q not found", username)
		}
		return err
	}

	// Create transaction
	var txID string
	if err := tx.QueryRow(ctx,
		`insert into transactions (reason, bet_id, note) values ('GIFT', null, $1) returning id`, note).
		Scan(&txID); err != nil {
		return err
	}

	// Balanced entries: house -> target
	if _, err := tx.Exec(ctx, `insert into ledger_entries (tx_id, account_id, delta) values ($1,$2,$3)`,
		txID, houseAccID, -amount); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `insert into ledger_entries (tx_id, account_id, delta) values ($1,$2,$3)`,
		txID, targetAccID, amount); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func giftToAllUsers(ctx context.Context, pool *pgxpool.Pool, amount int64, note string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	houseAccID, err := ensureHouseAccount(ctx, tx)
	if err != nil {
		return 0, fmt.Errorf("house account: %w", err)
	}

	// List all user default accounts, excluding house
	rows, err := tx.Query(ctx, `
		select u.id, a.id
		from users u
		join accounts a on a.user_id = u.id and a.is_default
		where u.username <> $1
	`, houseUsername)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type pair struct{ userID, accID string }
	var recips []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.userID, &p.accID); err != nil {
			return 0, err
		}
		recips = append(recips, p)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(recips) == 0 {
		return 0, fmt.Errorf("no recipients (only house exists?)")
	}

	total := amount * int64(len(recips))

	// Create single transaction with many entries
	var txID string
	if err := tx.QueryRow(ctx,
		`insert into transactions (reason, bet_id, note) values ('GIFT', null, $1) returning id`, note).
		Scan(&txID); err != nil {
		return 0, err
	}

	// House debit (negative)
	if _, err := tx.Exec(ctx, `insert into ledger_entries (tx_id, account_id, delta) values ($1,$2,$3)`,
		txID, houseAccID, -total); err != nil {
		return 0, err
	}
	// Recipients credit
	for _, p := range recips {
		if _, err := tx.Exec(ctx, `insert into ledger_entries (tx_id, account_id, delta) values ($1,$2,$3)`,
			txID, p.accID, amount); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(recips), nil
}

func ensureHouseAccount(ctx context.Context, tx pgx.Tx) (accountID string, err error) {
	// Check if house exists
	var houseID string
	err = tx.QueryRow(ctx, `select id from users where username=$1`, houseUsername).Scan(&houseID)
	if err == pgx.ErrNoRows {
		// Create house user with random password (not meant for login)
		pw := randomPassword(24)
		hash, err := auth.HashPassword(pw)
		if err != nil {
			return "", err
		}

		err = tx.QueryRow(ctx, `
			insert into users (username, display_name, password_hash, role)
			values ($1, $2, $3, 'admin')
			returning id
		`, houseUsername, "House", hash).Scan(&houseID)
		if err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}

	// Get its default wallet account (trigger should have created it)
	err = tx.QueryRow(ctx, `
		select id from accounts where user_id = $1 and is_default
	`, houseID).Scan(&accountID)
	if err == pgx.ErrNoRows {
		// Create explicitly if trigger didn’t (defensive)
		err = tx.QueryRow(ctx, `
			insert into accounts (user_id, name, is_default) values ($1, $2, true)
			returning id
		`, houseID, "wallet:"+houseUsername).Scan(&accountID)
	}
	return accountID, err
}

func randomPassword(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	if n <= 0 {
		n = 24
	}
	var b = make([]byte, n)
	for i := 0; i < n; i++ {
		idxBig, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		b[i] = alphabet[idxBig.Int64()]
	}
	return string(b)
}

func resolveDBURL(cfg *config.Config, override string) (string, error) {
	if strings.TrimSpace(override) != "" {
		return override, nil
	}
	return cfg.Database.AppURL()
}

func reorderArgs(args []string) []string {
	var flags []string
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if len(arg) > 0 && arg != "-" && arg != "--" && arg[0] == '-' {
			flags = append(flags, arg)
			if !strings.Contains(arg, "=") && i+1 < len(args) && (len(args[i+1]) == 0 || args[i+1][0] != '-') {
				flags = append(flags, args[i+1])
				i++
			}
		} else {
			positional = append(positional, arg)
		}
	}
	return append(flags, positional...)
}
