package config

import (
	"errors"
	"net/url"
	"strconv"
)

type Config struct {
	BaseURL string `yaml:"base_url"`

	HTTP struct {
		Address string `yaml:"address"`
	} `yaml:"http"`

	Database DatabaseConfig `yaml:"database"`

	Logging struct {
		Level  string `yaml:"level"`  // "debug" | "info" | "warn" | "error"
		Format string `yaml:"format"` // "text" | "json"
	} `yaml:"logging"`

	Security struct {
		JWTSecret string `yaml:"jwt_secret"`
	} `yaml:"security"`
}

type DatabaseConfig struct {
	URL      string `yaml:"url"`
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	Name     string `yaml:"name"`
	SSLMode  string `yaml:"sslmode"` // e.g. "disable" | "require"
}

func (c *Config) Defaults() {
	if c.HTTP.Address == "" {
		c.HTTP.Address = ":8080"
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Logging.Format == "" {
		c.Logging.Format = "json"
	}
	if c.Database.Host == "" {
		c.Database.Host = "db"
	}
	if c.Database.Port == 0 {
		c.Database.Port = 5432
	}
	if c.Database.User == "" {
		c.Database.User = "betsandpedestres"
	}
	if c.Database.Name == "" {
		c.Database.Name = "betsandpedestres"
	}
	if c.Database.Password == "" {
		c.Database.Password = "password"
	}
	if c.Database.SSLMode == "" {
		c.Database.SSLMode = "disable"
	}
	if c.Security.JWTSecret == "" {
		c.Security.JWTSecret = "change-me"
	}
}

func (c *Config) Validate() error {
	var errs []string
	// DB must have either URL or (Host, User, Name)
	if c.Database.URL == "" {
		if c.Database.Host == "" || c.Database.User == "" || c.Database.Name == "" {
			errs = append(errs, "database.url or database.{host,user,name} must be set")
		}
	}
	if len(errs) > 0 {
		return errors.New(joinErrs(errs))
	}
	return nil
}

func joinErrs(es []string) string {
	if len(es) == 1 {
		return es[0]
	}
	out := es[0]
	for i := 1; i < len(es); i++ {
		out += "; " + es[i]
	}
	return out
}

// AppURL returns a postgres connection URL for the application DB.
func (d *DatabaseConfig) AppURL() (string, error) {
	if d.URL != "" {
		return d.URL, nil
	}
	if d.Host == "" || d.User == "" || d.Name == "" {
		return "", errors.New("database config incomplete: need host, user, name or set url")
	}
	u := &url.URL{
		Scheme: "postgres",
		Host:   d.Host + ":" + strconv.Itoa(d.Port),
		Path:   "/" + d.Name,
	}
	if d.Password != "" {
		u.User = url.UserPassword(d.User, d.Password)
	} else {
		u.User = url.User(d.User)
	}
	q := url.Values{}
	if d.SSLMode != "" {
		q.Set("sslmode", d.SSLMode)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}
