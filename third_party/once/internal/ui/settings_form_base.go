package ui

import tea "charm.land/bubbletea/v2"

type settingsFormBase struct {
	form       Form
	title      string
	statusLine func() string
	viewFn     func(Form) string
}

func (b settingsFormBase) Title() string {
	return b.title
}

func (b settingsFormBase) Init() tea.Cmd {
	return b.form.Init()
}

func (b settingsFormBase) View() string {
	if b.viewFn != nil {
		return b.viewFn(b.form)
	}
	return b.form.View()
}

func (b settingsFormBase) StatusLine() string {
	if b.statusLine != nil {
		return b.statusLine()
	}
	return ""
}

// Private

func (b settingsFormBase) update(msg tea.Msg) (settingsFormBase, tea.Cmd) {
	var cmd tea.Cmd
	b.form, cmd = b.form.Update(msg)
	return b, cmd
}
