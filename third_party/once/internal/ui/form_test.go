package ui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
)

func TestForm_FocusCycling(t *testing.T) {
	form := NewForm("Submit",
		FormItem{Label: "First", Field: NewTextField("first")},
		FormItem{Label: "Second", Field: NewTextField("second")},
	)
	assert.Equal(t, 0, form.Focused())

	formPressTab(&form)
	assert.Equal(t, 1, form.Focused())

	formPressTab(&form)
	assert.Equal(t, 2, form.Focused(), "submit button")

	formPressTab(&form)
	assert.Equal(t, 3, form.Focused(), "cancel button")

	formPressTab(&form)
	assert.Equal(t, 0, form.Focused(), "wraps to first field")
}

func TestForm_ShiftTabCycling(t *testing.T) {
	form := NewForm("Submit",
		FormItem{Label: "First", Field: NewTextField("first")},
		FormItem{Label: "Second", Field: NewTextField("second")},
	)

	formPressShiftTab(&form)
	assert.Equal(t, 3, form.Focused(), "cancel button")

	formPressShiftTab(&form)
	assert.Equal(t, 2, form.Focused(), "submit button")

	formPressShiftTab(&form)
	assert.Equal(t, 1, form.Focused())

	formPressShiftTab(&form)
	assert.Equal(t, 0, form.Focused())
}

func TestForm_EnterAdvancesFocus(t *testing.T) {
	form := NewForm("Submit",
		FormItem{Label: "First", Field: NewTextField("first")},
		FormItem{Label: "Second", Field: NewTextField("second")},
	)

	formPressEnter(&form)
	assert.Equal(t, 1, form.Focused())

	formPressEnter(&form)
	assert.Equal(t, 2, form.Focused(), "submit button")
}

func TestForm_SubmitAction(t *testing.T) {
	form := NewForm("Done",
		FormItem{Label: "Field", Field: NewTextField("val")},
	)
	submitted := false
	form.OnSubmit(func(f *Form) tea.Cmd {
		submitted = true
		return nil
	})

	formPressTab(&form)
	assert.Equal(t, 1, form.Focused(), "submit button")

	form, _ = form.Update(keyPressMsg("enter"))
	assert.True(t, submitted)
}

func TestForm_CancelAction(t *testing.T) {
	form := NewForm("Done",
		FormItem{Label: "Field", Field: NewTextField("val")},
	)
	cancelled := false
	form.OnCancel(func(f *Form) tea.Cmd {
		cancelled = true
		return nil
	})

	formPressTab(&form)
	formPressTab(&form)
	assert.Equal(t, 2, form.Focused(), "cancel button")

	form, _ = form.Update(keyPressMsg("enter"))
	assert.True(t, cancelled)
}

func TestForm_NoFields(t *testing.T) {
	form := NewForm("Done")
	assert.Equal(t, 0, form.Focused(), "submit button")

	formPressTab(&form)
	assert.Equal(t, 1, form.Focused(), "cancel button")

	formPressTab(&form)
	assert.Equal(t, 0, form.Focused(), "wraps to submit")
}

func TestTextField_DigitsOnly(t *testing.T) {
	field := NewTextField("number")
	field.SetDigitsOnly(true)
	field.Focus()

	field.Update(keyPressMsg("5"))
	assert.Equal(t, "5", field.Value())

	field.Update(keyPressMsg("a"))
	assert.Equal(t, "5", field.Value(), "non-digit rejected")

	field.Update(keyPressMsg("3"))
	assert.Equal(t, "53", field.Value())
}

func TestCheckboxField_Toggle(t *testing.T) {
	field := NewCheckboxField("Enable", false)
	assert.False(t, field.Checked())

	field.Update(keyPressMsg("space"))
	assert.True(t, field.Checked())

	field.Update(keyPressMsg("space"))
	assert.False(t, field.Checked())
}

func TestCheckboxField_Render(t *testing.T) {
	field := NewCheckboxField("TLS", true)
	assert.Equal(t, "[✓] TLS", field.View())

	field.Update(keyPressMsg("space"))
	assert.Equal(t, "[ ] TLS", field.View())
}

func TestCheckboxField_DisabledWhen(t *testing.T) {
	disabled := true
	field := NewCheckboxField("TLS", false)
	field.SetDisabledWhen(func() (bool, string) {
		return disabled, "Not available"
	})

	field.Update(keyPressMsg("space"))
	assert.False(t, field.Checked(), "toggle ignored when disabled")
	assert.Equal(t, "Not available", field.View())

	disabled = false
	field.Update(keyPressMsg("space"))
	assert.True(t, field.Checked(), "toggle works when enabled")
	assert.Equal(t, "[✓] TLS", field.View())
}

func TestForm_FieldValuesAccessible(t *testing.T) {
	form := NewForm("Submit",
		FormItem{Label: "Name", Field: NewTextField("name")},
	)

	formTypeText(&form, "hello")
	assert.Equal(t, "hello", form.TextField(0).Value())
}

func TestForm_ValidationBlocksSubmitWhenRequiredEmpty(t *testing.T) {
	form := NewForm("Submit",
		FormItem{Label: "Name", Field: NewTextField("name"), Required: true},
	)
	submitted := false
	form.OnSubmit(func(f *Form) tea.Cmd {
		submitted = true
		return nil
	})

	formFocusSubmit(&form)
	formPressEnter(&form)

	assert.False(t, submitted)
	assert.True(t, form.HasError())
	assert.Equal(t, "Name is required", form.Error())
}

func TestForm_ValidationAllowsSubmitWhenRequiredFilled(t *testing.T) {
	form := NewForm("Submit",
		FormItem{Label: "Name", Field: NewTextField("name"), Required: true},
	)
	submitted := false
	form.OnSubmit(func(f *Form) tea.Cmd {
		submitted = true
		return nil
	})

	formTypeText(&form, "hello")
	formFocusSubmit(&form)
	formPressEnter(&form)

	assert.True(t, submitted)
	assert.False(t, form.HasError())
}

func TestForm_ValidationTreatsWhitespaceAsEmpty(t *testing.T) {
	form := NewForm("Submit",
		FormItem{Label: "Name", Field: NewTextField("name"), Required: true},
	)
	submitted := false
	form.OnSubmit(func(f *Form) tea.Cmd {
		submitted = true
		return nil
	})

	formTypeText(&form, "   ")
	formFocusSubmit(&form)
	formPressEnter(&form)

	assert.False(t, submitted)
	assert.Equal(t, "Name is required", form.Error())
}

func TestForm_ValidationErrorClearsOnInput(t *testing.T) {
	form := NewForm("Submit",
		FormItem{Label: "Name", Field: NewTextField("name"), Required: true},
	)
	form.OnSubmit(func(f *Form) tea.Cmd { return nil })

	formFocusSubmit(&form)
	formPressEnter(&form)

	assert.True(t, form.HasError())
	assert.Equal(t, 0, form.Focused())

	formTypeText(&form, "x")

	assert.False(t, form.HasError())
}

func TestForm_ValidationNonRequiredDoesNotBlock(t *testing.T) {
	form := NewForm("Submit",
		FormItem{Label: "Optional", Field: NewTextField("opt")},
		FormItem{Label: "Required", Field: NewTextField("req"), Required: true},
	)
	submitted := false
	form.OnSubmit(func(f *Form) tea.Cmd {
		submitted = true
		return nil
	})

	formPressTab(&form) // focus second field
	formTypeText(&form, "filled")
	formFocusSubmit(&form)
	formPressEnter(&form)

	assert.True(t, submitted)
	assert.False(t, form.HasError())
}

func TestForm_ValidationOnClickSubmit(t *testing.T) {
	form := NewForm("Submit",
		FormItem{Label: "Name", Field: NewTextField("name"), Required: true},
	)
	submitted := false
	form.OnSubmit(func(f *Form) tea.Cmd {
		submitted = true
		return nil
	})

	formClickSubmit(&form)

	assert.False(t, submitted)
	assert.True(t, form.HasError())
}

func TestForm_ValidationFocusesFirstError(t *testing.T) {
	form := NewForm("Submit",
		FormItem{Label: "First", Field: NewTextField("first")},
		FormItem{Label: "Second", Field: NewTextField("second"), Required: true},
		FormItem{Label: "Third", Field: NewTextField("third"), Required: true},
	)
	form.OnSubmit(func(f *Form) tea.Cmd { return nil })

	formFocusSubmit(&form)
	formPressEnter(&form)

	assert.Equal(t, 1, form.Focused(), "focused on first errored field")
	assert.Equal(t, "Second is required", form.Error())
}

func TestFormOnRebuild_CalledOnKeyPress(t *testing.T) {
	form := NewForm("Done",
		FormItem{Label: "Field", Field: NewTextField("val")},
	)
	called := false
	form.OnRebuild(func(f *Form) { called = true })

	formTypeText(&form, "x")
	assert.True(t, called)
}

func TestFormOnRebuild_NotCalledOnTab(t *testing.T) {
	form := NewForm("Done",
		FormItem{Label: "First", Field: NewTextField("first")},
		FormItem{Label: "Second", Field: NewTextField("second")},
	)
	called := false
	form.OnRebuild(func(f *Form) { called = true })

	formPressTab(&form)
	assert.False(t, called)
}

func TestFormOnRebuild_NotCalledOnWindowSize(t *testing.T) {
	form := NewForm("Done",
		FormItem{Label: "Field", Field: NewTextField("val")},
	)
	called := false
	form.OnRebuild(func(f *Form) { called = true })

	form, _ = form.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	assert.False(t, called)
}

func TestFormAppendItems(t *testing.T) {
	form := NewForm("Done",
		FormItem{Label: "First", Field: NewTextField("first")},
	)
	assert.Equal(t, 1, form.ItemCount())

	form.AppendItems(
		FormItem{Label: "Second", Field: NewTextField("second")},
		FormItem{Label: "Third", Field: NewTextField("third")},
	)
	assert.Equal(t, 3, form.ItemCount())
	assert.Equal(t, "", form.TextField(1).Value())
	assert.Equal(t, "", form.TextField(2).Value())
}

func TestFormAppendItems_FocusStability(t *testing.T) {
	form := NewForm("Done",
		FormItem{Label: "First", Field: NewTextField("first")},
		FormItem{Label: "Second", Field: NewTextField("second")},
	)
	formPressTab(&form)
	assert.Equal(t, 1, form.Focused())

	form.AppendItems(FormItem{Label: "Third", Field: NewTextField("third")})
	assert.Equal(t, 1, form.Focused(), "focus unchanged after append")
}

// Helpers

func updateForm(f Form, msg tea.Msg) Form {
	f, _ = f.Update(msg)
	return f
}

func formPressTab(form *Form) {
	*form = updateForm(*form, keyPressMsg("tab"))
}

func formPressShiftTab(form *Form) {
	*form = updateForm(*form, keyPressMsg("shift+tab"))
}

func formPressEnter(form *Form) {
	*form = updateForm(*form, keyPressMsg("enter"))
}

func formTypeText(form *Form, text string) {
	for _, r := range text {
		*form = updateForm(*form, keyPressMsg(string(r)))
	}
}

func formFocusSubmit(form *Form) {
	for form.Focused() != form.submitIndex() {
		formPressTab(form)
	}
}

func formClickSubmit(form *Form) {
	*form = updateForm(*form, MouseEvent{IsClick: true, Target: "submit"})
}
