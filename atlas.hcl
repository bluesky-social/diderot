env "local" {
  url     = "postgres://diderot:diderot@localhost:15432/diderot?sslmode=disable&search_path=public"
  dev     = "docker://postgres/18/dev?search_path=public"
  migration {
    dir = "file://internal/migrations/sql"
  }
}

env "prod" {
  # Atlas CLI path only (e.g. `atlas migrate status --env prod`).
  # The diderot binary runs migrations in-process on startup using
  # the same DBURL, so there is no separate migrate role.
  url = getenv("DIDEROT_DB_URL")
  migration {
    dir = "file://internal/migrations/sql"
  }
}

lint {
  destructive {
    error = true
  }
  data_depend {
    error = true
  }
}
