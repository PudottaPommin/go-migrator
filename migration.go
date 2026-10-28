package migrator

type Migration struct {
	Version   int
	Up        string
	Down      string
	DisableTx bool
}

func NewMigration(id int) *Migration {
	return &Migration{
		Version:   id,
		Up:        "",
		Down:      "",
		DisableTx: false,
	}
}

func (m *Migration) reversible() bool {
	return m.Down != ""
}
