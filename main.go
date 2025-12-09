package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/log"
	"github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	"github.com/charmbracelet/wish/activeterm"
	"github.com/charmbracelet/wish/bubbletea"
	"github.com/charmbracelet/wish/logging"
	"github.com/muesli/termenv"
)

const (
	maxConcurrentConns = 50
	maxIdleTime        = 30 * time.Minute
	rateLimitWindow    = 1 * time.Minute
	maxConnPerIP       = 5
)

// Connection tracking for rate limiting
type connectionTracker struct {
	mu          sync.RWMutex
	connections map[string][]time.Time
	active      int
}

func newConnectionTracker() *connectionTracker {
	return &connectionTracker{
		connections: make(map[string][]time.Time),
	}
}

func (ct *connectionTracker) canConnect(ip string) bool {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	// Check total active connections
	if ct.active >= maxConcurrentConns {
		log.Warn("Max concurrent connections reached", "active", ct.active)
		return false
	}

	// Clean old entries
	now := time.Now()
	times := ct.connections[ip]
	var recentTimes []time.Time
	for _, t := range times {
		if now.Sub(t) < rateLimitWindow {
			recentTimes = append(recentTimes, t)
		}
	}
	ct.connections[ip] = recentTimes

	// Check rate limit
	if len(recentTimes) >= maxConnPerIP {
		log.Warn("Rate limit exceeded", "ip", ip, "connections", len(recentTimes))
		return false
	}

	return true
}

func (ct *connectionTracker) addConnection(ip string) {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	ct.connections[ip] = append(ct.connections[ip], time.Now())
	ct.active++
}

func (ct *connectionTracker) removeConnection() {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	if ct.active > 0 {
		ct.active--
	}
}

var connTracker = newConnectionTracker()

func init() {
	lipgloss.SetColorProfile(termenv.ColorProfile())
}

func main() {
	host := os.Getenv("HOST")
	if host == "" {
		host = "0.0.0.0"
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "22"
	}
	if _, err := os.Stat("content"); os.IsNotExist(err) {
		log.Fatal("Content directory not found", "error", err)
	}

	logLevel := log.InfoLevel
	if os.Getenv("DEBUG") == "true" {
		logLevel = log.DebugLevel
	}
	log.SetLevel(logLevel)

	s, err := wish.NewServer(
		wish.WithAddress(net.JoinHostPort(host, port)),
		wish.WithHostKeyPath("/data/ssh_host_key"),
		wish.WithIdleTimeout(maxIdleTime),
		wish.WithMaxTimeout(2*time.Hour),
		wish.WithMiddleware(
			rateLimitMiddleware(),
			bubbletea.Middleware(teaHandler),
			activeterm.Middleware(),
			logging.Middleware(),
		),
	)
	if err != nil {
		log.Error("Could not start server", "error", err)
		os.Exit(1)

	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	log.Info("Starting SSH server", "host", host, "port", port)
	go func() {
		if err = s.ListenAndServe(); err != nil && !errors.Is(err, ssh.ErrServerClosed) {
			log.Error("Could not start server", "error", err)
			done <- nil
		}
	}()

	<-done
	log.Info("Stopping SSH server")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer func() { cancel() }()
	if err := s.Shutdown(ctx); err != nil && !errors.Is(err, ssh.ErrServerClosed) {
		log.Error("Could not stop server", "error", err)
	}
	log.Info("Server stopped")
}

// Rate limiting middleware
func rateLimitMiddleware() wish.Middleware {
	return func(next ssh.Handler) ssh.Handler {
		return func(s ssh.Session) {
			ip := getClientIP(s)

			if !connTracker.canConnect(ip) {
				log.Warn("Connection rejected", "ip", ip, "reason", "rate_limit")
				s.Write([]byte("\r\n⚠️  Rate limit exceeded. Please try again later.\r\n\r\n"))
				s.Exit(1)
				return
			}

			connTracker.addConnection(ip)
			defer connTracker.removeConnection()

			next(s)
		}
	}
}

func getClientIP(s ssh.Session) string {
	addr := s.RemoteAddr().String()
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

func getVersion() string {
	version := os.Getenv("APP_VERSION")
	if version == "" {
		version = "dev"
	}
	return version
}

func teaHandler(s ssh.Session) (tea.Model, []tea.ProgramOption) {
	pty, _, _ := s.Pty()

	if pty.Window.Width < 80 || pty.Window.Height < 25 {
		log.Warn("Client terminal too small",
			"user", s.User(),
			"ip", getClientIP(s),
			"size", fmt.Sprintf("%dx%d", pty.Window.Width, pty.Window.Height))
	}
	renderer := bubbletea.MakeRenderer(s)
	lipgloss.SetColorProfile(renderer.ColorProfile())

	darkBg := renderer.HasDarkBackground()

	m := initializeModel()

	//logging session start
	log.Info("New SSH session",
		"user", s.User(),
		"remote", s.RemoteAddr().String(),
		"term", pty.Term,
		"size", fmt.Sprintf("%dx%d", pty.Window.Width, pty.Window.Height),
		"colors", renderer.ColorProfile())

	bg := "light"
	if darkBg {
		bg = "dark"
	}
	_ = bg
	return m, []tea.ProgramOption{tea.WithAltScreen(), tea.WithMouseCellMotion()}
}

const SidebarWidth = 34

type FocusArea int

const (
	FocusSidebar FocusArea = iota
	FocusProject
)

var (
	green       = lipgloss.Color("#21c48b")
	red         = lipgloss.Color("#ff2c2c")
	teal        = lipgloss.Color("#3bd2c9")
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(green).PaddingLeft(1)

	sidebarStyle = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder()).
			BorderForeground(lipgloss.Color("#243b3a")).
			Padding(1).
			MarginRight(1)

	sideItemStyle  = lipgloss.NewStyle().Padding(0, 1)
	highlightStyle = lipgloss.NewStyle().
			Bold(true).Foreground(teal).
			Background(lipgloss.Color("#042822")).
			Padding(0, 1)

	mainStyle = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder()).
			BorderForeground(lipgloss.Color("#263737")).
			Padding(1)

	statusStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#8fb9a8")).
			PaddingLeft(1).Height(1)

	cardStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(teal).
			Padding(1).
			Margin(1, 0)

	visitButton = lipgloss.NewStyle().
			Background(teal).Foreground(lipgloss.Color("#00100f")).
			Bold(true).Padding(0, 1)
)

type model struct {
	choices        []string
	cursor         int
	width          int
	height         int
	ready          bool
	startedAt      time.Time
	contents       map[string]string
	contact        string
	projectCursor  int
	viewportWidth  int
	viewportHeight int
	focus          FocusArea
	showHelp       bool
	showURLPopup   bool
	selectedURL    string

	vp     viewport.Model
	helpVp viewport.Model // Add this line

}

type Project struct {
	Title       string
	Description string
	Tech        string
	Link        string
}

func initializeModel() *model {
	choices := []string{"Home", "Project", "Skill", "Experience", "About"}
	contactData, err := os.ReadFile("content/contact.md")
	if err != nil {
		log.Warn("Failed to load contact.md", "error", err)
		contactData = []byte("# Contact\n\nContact information unavailable")
	}
	return &model{
		choices:        choices,
		cursor:         0,
		startedAt:      time.Now(),
		contents:       loadAllContent(choices),
		contact:        string(contactData),
		viewportWidth:  80,
		viewportHeight: 20,
		focus:          FocusSidebar,
		showURLPopup:   false,
		selectedURL:    "",
	}
}

func loadAllContent(sections []string) map[string]string {
	data := make(map[string]string)
	for _, section := range sections {
		filename := "content/" + strings.ToLower(section) + ".md"
		content, err := os.ReadFile(filename)
		if err != nil {
			data[section] = fmt.Sprintf("# %s\n\n(Unable to load %s)", section, filename)
			continue
		}
		data[section] = string(content)
	}
	return data
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(tea.EnterAltScreen, tea.ClearScreen)
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch t := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = t.Width
		m.height = t.Height

		// responsive math (account for borders/padding)
		sideWidth := SidebarWidth
		mainWidth := m.width - sideWidth - 6
		mainHeight := m.height - 5
		if mainWidth < 10 {
			mainWidth = 10
		}
		if mainHeight < 3 {
			mainHeight = 3
		}

		// Account for mainStyle border and padding 
		viewportWidth := mainWidth - 4
		viewportHeight := mainHeight - 2
		if viewportWidth < 6 {
			viewportWidth = 6
		}
		if viewportHeight < 1 {
			viewportHeight = 1
		}

		m.viewportWidth = viewportWidth
		m.viewportHeight = viewportHeight

		if !m.ready {
			m.vp = viewport.New(viewportWidth, viewportHeight)
			m.ready = true
			m.resetViewportContent()
		} else {
			m.vp.Width = viewportWidth
			m.vp.Height = viewportHeight
			m.resetViewportContent()
		}

	case tea.KeyMsg:
		k := t.String()
		if m.showURLPopup {
			switch k {
			case "ctrl+c", "q", "esc", "enter":
				m.showURLPopup = false
				m.selectedURL = ""
				return m, nil
			}
			return m, nil
		}

		if m.showHelp {
			switch k {
			case "ctrl+c", "q":
				return m, tea.Quit
			case "?", "esc":
				m.showHelp = false
				return m, nil
			case "up", "k":
				m.helpVp.ScrollUp(1)
				return m, nil
			case "down", "j":
				m.helpVp.ScrollDown(1)
				return m, nil
			case "pgup", "pageup":
				m.helpVp.PageUp()
				return m, nil
			case "pgdown", "pagedown":
				m.helpVp.PageDown()
				return m, nil
			case "shift+up":
				m.helpVp.ScrollUp(3)
				return m, nil
			case "shift+down":
				m.helpVp.ScrollDown(3)
				return m, nil
			case "g":
				m.helpVp.GotoTop()
				return m, nil
			case "G":
				m.helpVp.GotoBottom()
				return m, nil

			}
			return m, nil
		}
		// Toggle focus: Tab
		if k == "tab" {
			// Only toggle to project if the selected section is Project
			if m.choices[m.cursor] == "Project" {
				if m.focus == FocusSidebar {
					m.focus = FocusProject
				} else {
					m.focus = FocusSidebar
				}
				m.resetViewportContent()
				return m, nil
			}
		}

		// Allow escaping focus back to sidebar via Esc / left / h
		if k == "esc" || k == "left" || k == "h" {
			if m.showHelp {
				m.showHelp = false // Close help with Esc
				return m, nil
			}

			if m.focus == FocusProject {
				m.focus = FocusSidebar
				m.resetViewportContent()
				return m, nil
			}
		}

		// PROJECT PAGE KEY HANDLING (only when project is focused)
		if m.choices[m.cursor] == "Project" && m.focus == FocusProject {
			projects := parseProjects(m.contents["Project"])

			switch k {
			case "up", "k":
				if m.projectCursor > 0 {
					m.projectCursor--
					m.resetViewportContent()
					m.scrollToSelectedProject()
				}
				return m, nil

			case "down", "j":
				if m.projectCursor < len(projects)-1 {
					m.projectCursor++
					m.resetViewportContent()
					m.scrollToSelectedProject()
				}
				return m, nil

			case "enter":
				if m.projectCursor >= 0 && m.projectCursor < len(projects) {
					m.showURLPopup = true
					m.selectedURL = projects[m.projectCursor].Link
				}
				return m, nil
			case "pgdown", "pagedown":
				m.vp.PageDown()
				return m, nil
			case "pgup", "pageup":
				m.vp.PageUp()
				return m, nil
			case "shift+down":
				m.vp.ScrollDown(6)
				return m, nil
			case "shift+up":
				m.vp.ScrollUp(6)
				return m, nil
			}
		}

		// Global keys
		switch k {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "?":
			m.showHelp = !m.showHelp
			if m.showHelp {
				// Reset to top when opening help
				m.helpVp.GotoTop()
			}
			return m, nil
		}

		// Sidebar navigation applies only when sidebar is focused
		switch k {
		case "up", "k":
			if m.focus == FocusSidebar {
				if m.cursor > 0 {
					m.cursor--
					// reset scroll to top when changing selection
					m.projectCursor = 0
					m.resetViewportContent()
				}
			} else {
				// when project is focused but key not handled above, ignore
			}
		case "down", "j":
			if m.focus == FocusSidebar {
				if m.cursor < len(m.choices)-1 {
					m.cursor++
					m.projectCursor = 0
					m.resetViewportContent()
				}
			}
		case "pgdown", "pagedown":
			if m.focus != FocusProject {
				m.vp.PageDown()
			}
		case "pgup", "pageup":
			if m.focus != FocusProject {
				m.vp.PageUp()
			}
		case "shift+up":
			if m.focus != FocusProject {
				m.vp.ScrollUp(6)
			}
		case "shift+down":
			if m.focus != FocusProject {
				m.vp.ScrollDown(6)
			}
		}
	}

	return m, nil
}

func (m *model) resetViewportContent() {
	if !m.ready {
		return
	}

	// ALWAYS regenerate full UI to update highlights
	fullContent := m.buildFullUI(m.width)
	m.vp.SetContent(fullContent)

	// For project focus, scroll to selected project
	if m.choices[m.cursor] == "Project" && m.focus == FocusProject {
		m.scrollToSelectedProject()
	} else {
		m.vp.GotoTop()
	}
}

func (m *model) scrollToSelectedProject() {
	if !m.ready {
		return
	}

	projects := parseProjects(m.contents["Project"])
	if len(projects) == 0 || m.projectCursor < 0 || m.projectCursor >= len(projects) {
		return
	}

	// Estimate lines per card (adjust based on your average card size)
	const estimatedCardHeight = 12
	const headerHeight = 4

	// Calculate approximate line position of selected card
	selectedCardLine := headerHeight + (m.projectCursor * estimatedCardHeight)

	currentOffset := m.vp.YOffset
	viewportHeight := m.vp.Height

	// Define visible range with some padding
	visibleTop := currentOffset + 2 // Add small padding
	visibleBottom := currentOffset + viewportHeight - 2

	selectedCardBottom := selectedCardLine + estimatedCardHeight

	// Only scroll if selected card is outside visible area
	if selectedCardLine < visibleTop {
		// Card is above visible area - scroll up
		m.vp.SetYOffset(selectedCardLine - 2)
	} else if selectedCardBottom > visibleBottom {
		// Card is below visible area - scroll down
		newOffset := selectedCardBottom - viewportHeight + 10
		if newOffset < 0 {
			newOffset = 0
		}
		m.vp.SetYOffset(newOffset)
	}
	// If card is already visible, don't scroll
}

func (m *model) View() string {
	if !m.ready {
		return "\n Loading...\n"
	}

	termW := m.width
	termH := m.height

	const minWidth = 80
	const minHeight = 25

	if termW < minWidth || termH < minHeight {
		return m.renderTooSmallMessage(termW, termH, minWidth, minHeight)
	}

	if termW <= 0 || termH <= 0 {
		return "Waiting for terminal size..."
	}

	// Show help overlay if active
	if m.showHelp {
		return m.renderHelpOverlay(termW, termH)
	}

	// Showing URL popup if active
	if m.showURLPopup {
		return m.renderURLPopup(termW, termH)
	}

	// Build the full UI content (no height restrictions)
	fullContent := m.buildFullUI(termW)

	// Calculate viewport height (leave room for footer)
	const footerHeight = 3 // status + help

	viewportHeight := termH - footerHeight

	// Update main viewport to show full UI
	m.vp.Width = termW
	m.vp.Height = viewportHeight
	m.vp.SetContent(fullContent)

	// Footer
	status := m.renderStatus(termW-4, termW, termH)
	help := lipgloss.NewStyle().Foreground(lipgloss.Color("#9fbfb0")).Render(
		" ↑/↓: Navigate • PgUp/PgDown or Shift+↑/Shift+↓: Scroll • Tab: Switch Panel • q: Quit ",
	)

	return m.vp.View() + "\n" + status + "\n" + help
}

func (m *model) buildFullUI(termW int) string {
	const containerPadX = 2
	usableW := termW - containerPadX*2

	// Sidebar dimensions
	sidebarRatio := 0.27
	sidebarOuterW := int(float64(usableW) * sidebarRatio)
	if sidebarOuterW < 28 {
		sidebarOuterW = 28
	}
	if sidebarOuterW > int(float64(usableW)*0.45) {
		sidebarOuterW = int(float64(usableW) * 0.45)
	}

	gap := 1
	mainOuterW := usableW - sidebarOuterW - gap

	if mainOuterW < 40 {
		needed := 40 - mainOuterW
		sidebarOuterW -= needed
		if sidebarOuterW < 24 {
			sidebarOuterW = 24
		}
		mainOuterW = usableW - sidebarOuterW - gap
		if mainOuterW < 40 {
			mainOuterW = 40
		}
	}

	// Build sidebar and main content (natural height)
	sidebar := m.renderSidebarNatural(sidebarOuterW)
	mainContent := m.renderMainContent(mainOuterW)

	row := lipgloss.JoinHorizontal(lipgloss.Top,
		sidebar,
		lipgloss.NewStyle().Width(gap).Render(""),
		mainContent,
	)

	const containerPadY = 1
	return lipgloss.NewStyle().Padding(containerPadY, containerPadX, 0, containerPadX).Render(row)
}

// Add this new helper function
func (m *model) renderTooSmallMessage(currentW, currentH, minW, minH int) string {
	boxStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#ff6b6b")).
		Bold(true).
		Padding(1, 2).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#ff6b6b")).
		Align(lipgloss.Center)

	titleStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#ffd93d")).
		Bold(true)

	infoStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#a8dadc"))

	message := fmt.Sprintf(
		"%s\n\n"+
			"%s\n"+
			"%s\n\n"+
			"%s",
		titleStyle.Render("⚠️  Terminal Too Small"),
		infoStyle.Render(fmt.Sprintf("Current size: %d x %d", currentW, currentH)),
		infoStyle.Render(fmt.Sprintf("Minimum required: %d x %d", minW, minH)),
		lipgloss.NewStyle().Foreground(lipgloss.Color("#8fb9a8")).Render("Please resize your terminal window or press 'q' to quit"),
	)

	return "\n\n" + boxStyle.Render(message) + "\n\n"
}

func (m *model) renderURLPopup(termW, termH int) string {
	popupWidth := termW - 10
	if popupWidth > 80 {
		popupWidth = 80
	}
	if popupWidth < 40 {
		popupWidth = 40
	}

	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(green).
		Align(lipgloss.Center).
		MarginBottom(1)

	instructionStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#8fb9a8")).
		Align(lipgloss.Center).
		MarginTop(1)

	content := titleStyle.Render("🔗 Project Link") + "\n\n" +
		m.selectedURL + "\n\n" +
		instructionStyle.Render("Copy this URL to visit the project\n(Press Enter or Esc to close)")

	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(teal).
		Padding(2, 3).
		Width(popupWidth)

	popup := boxStyle.Render(content)

	// Center the popup
	centered := lipgloss.Place(termW, termH, lipgloss.Center, lipgloss.Center, popup)

	return centered
}

func (m *model) renderSidebarNatural(w int) string {
	const sidebarInnerSlack = 4
	innerW := w - sidebarInnerSlack
	if innerW < 16 {
		innerW = 16
	}

	var out strings.Builder
	for i, c := range m.choices {
		cursor := "  "
		if i == m.cursor {
			if m.focus == FocusSidebar {
				cursor = "▶ "
			} else {
				cursor = "▷ "
			}
		}

		line := cursor + c
		if i == m.cursor {
			out.WriteString(highlightStyle.Render(line) + "\n")
		} else {
			out.WriteString(sideItemStyle.Render(line) + "\n")
		}
	}

	navList := strings.TrimSpace(out.String())
	contactBox := m.renderContactBox(innerW)

	sidebarContent := navList + "\n\n" + contactBox

	// Natural height - no restriction
	return sidebarStyle.Width(w).Render(sidebarContent)
}

func (m *model) renderMainContent(w int) string {
	title := m.choices[m.cursor]
	body := m.contents[title]

	// PROJECT PAGE
	if title == "Project" {
		return m.renderProjectsNatural(w, body)
	}

	// NON-PROJECT SECTIONS
	renderer, err := glamour.NewTermRenderer(
		glamour.WithAutoStyle(),
		glamour.WithWordWrap(w-4),
	)

	var rendered string
	if err == nil {
		r, err := renderer.Render(body)
		if err == nil {
			rendered = r
		} else {
			rendered = body
		}
	} else {
		rendered = body
	}

	content := headerStyle.Render(" "+title) + "\n\n" + rendered
	return mainStyle.Width(w).Render(content)
}

func (m *model) renderProjectsNatural(w int, body string) string {
	projects := parseProjects(body)

	availableWidth := w - 6
	minCardWidth := 30
	maxCardWidth := 68
	cardWidth := availableWidth

	if cardWidth > maxCardWidth {
		cardWidth = maxCardWidth
	}
	if cardWidth < minCardWidth {
		cardWidth = minCardWidth
	}
	if cardWidth > availableWidth {
		cardWidth = availableWidth
	}
	var wrappedCards []string

	// Build and style each card individually
	for i, p := range projects {
		// Determine button style based on focus and selection
		var btn string
		if m.focus == FocusProject && i == m.projectCursor {
			btn = highlightStyle.Render(" Press Enter to view link  → ")
		} else {
			btn = visitButton.Render(" Visit → ")
		}

		textWidth := cardWidth - 6
		if textWidth < 20 {
			textWidth = 20
		}
		wrappedTitle := wrapText(p.Title, textWidth)
		wrappedDesc := wrapText(p.Description, textWidth)
		techText := "Tech: " + p.Tech
		wrappedTech := wrapText(techText, textWidth)

		// Build card content
		cardContent := fmt.Sprintf(
			"%s\n\n%s\n\n%s\n\n%s",
			wrappedTitle,
			wrappedDesc,
			wrappedTech,
			btn,
		)

		// Apply card styling with potential highlight
		var styledCard string
		if m.focus == FocusProject && i == m.projectCursor {
			// Selected card - use highlighted border
			selectedCardStyle := cardStyle.
				BorderForeground(red)
			styledCard = selectedCardStyle.Width(cardWidth).Render(cardContent)
		} else {
			// Normal card
			styledCard = cardStyle.Width(cardWidth).Render(cardContent)
		}

		// Center the card
		centered := lipgloss.PlaceHorizontal(w-4, lipgloss.Center, styledCard)
		wrappedCards = append(wrappedCards, centered)
	}

	// Join all cards with newlines
	allCards := strings.Join(wrappedCards, "\n")

	content := headerStyle.Render(" Project") + "\n\n" + allCards
	return mainStyle.Width(w).Render(content)
}

func wrapText(text string, width int) string {
	if width <= 0 {
		width = 20
	}

	words := strings.Fields(text)
	if len(words) == 0 {
		return text
	}

	var lines []string
	var currentLine string

	for _, word := range words {
		// If adding this word would exceed width
		testLine := currentLine
		if testLine != "" {
			testLine += " "
		}
		testLine += word

		if len(testLine) <= width {
			currentLine = testLine
		} else {
			// If current line has content, save it
			if currentLine != "" {
				lines = append(lines, currentLine)
				currentLine = word
			} else {
				// Word itself is longer than width, break it
				if len(word) > width {
					// Split the long word
					for len(word) > width {
						lines = append(lines, word[:width])
						word = word[width:]
					}
					currentLine = word
				} else {
					currentLine = word
				}
			}
		}
	}

	// Add remaining line
	if currentLine != "" {
		lines = append(lines, currentLine)
	}

	return strings.Join(lines, "\n")
}

func (m *model) renderStatus(w int, termW int, termH int) string {
	uptime := time.Since(m.startedAt).Truncate(time.Second).String()

	// Left side: status info
	leftSide := fmt.Sprintf("Ready | Uptime: %s | Size: %dx%d", uptime, termW, termH)

	// Right side: help icon
	rightSide := " ? Help "

	// Calculate spacing
	totalContent := len(leftSide) + len(rightSide)
	spacing := ""
	if w > totalContent {
		spacing = strings.Repeat(" ", w-totalContent)
	}

	line := leftSide + spacing + rightSide
	return statusStyle.Width(w).Render(line)
}

func (m *model) renderHelpOverlay(termW, termH int) string {
	helpWidth := termW - 10
	if helpWidth > 80 {
		helpWidth = 80
	}
	if helpWidth < 40 {
		helpWidth = 40
	}

	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(green).
		Align(lipgloss.Center).
		MarginBottom(1)

	sectionStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(teal).
		MarginTop(1)

	keyStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("#21c48b")).
		Width(20)

	descStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#e8f4f0"))

	helpContent := titleStyle.Render("📚 Portfolio TUI - Help Guide") + "\n\n"

	// Navigation Section
	helpContent += sectionStyle.Render("🧭 Navigation") + "\n"
	helpContent += keyStyle.Render("↑/k, ↓/j") + descStyle.Render("Navigate menu items in sidebar") + "\n"
	helpContent += keyStyle.Render("Tab") + descStyle.Render("Switch between sidebar and project panel") + "\n"
	helpContent += keyStyle.Render("Esc / ← / h") + descStyle.Render("Return to sidebar from project panel") + "\n\n"

	// Scrolling Section
	helpContent += sectionStyle.Render("📜 Scrolling") + "\n"
	helpContent += keyStyle.Render("Shift+↑") + descStyle.Render("Scroll up one line") + "\n"
	helpContent += keyStyle.Render("Shift+↓") + descStyle.Render("Scroll down one line") + "\n"
	helpContent += keyStyle.Render("PgUp") + descStyle.Render("Scroll up one page") + "\n"
	helpContent += keyStyle.Render("PgDown") + descStyle.Render("Scroll down one page") + "\n\n"

	// Project Section
	helpContent += sectionStyle.Render("🚀 Projects") + "\n"
	helpContent += keyStyle.Render("Tab") + descStyle.Render("Enter project selection mode") + "\n"
	helpContent += keyStyle.Render("↑/k, ↓/j") + descStyle.Render("Navigate between projects") + "\n"
	helpContent += keyStyle.Render("Enter") + descStyle.Render("Open project link in browser") + "\n\n"

	// General Section
	helpContent += sectionStyle.Render("⚙️  General") + "\n"
	helpContent += keyStyle.Render("?") + descStyle.Render("Toggle this help screen") + "\n"
	helpContent += keyStyle.Render("q / Ctrl+C") + descStyle.Render("Quit application") + "\n\n"

	// Workflow Section
	helpContent += sectionStyle.Render("💡 How to Use") + "\n"
	helpContent += descStyle.Render("1. Use ↑/↓ to browse sections (Home, Project, Skill, etc.)") + "\n"
	helpContent += descStyle.Render("2. Press Tab when on 'Project' to interact with projects") + "\n"
	helpContent += descStyle.Render("3. Select a project and press Enter to visit its link") + "\n"
	helpContent += descStyle.Render("4. Use Shift+↑/↓ or PgUp/PgDown to scroll long content") + "\n"
	helpContent += descStyle.Render("5. Check the contact box in sidebar for ways to reach out!") + "\n"

	// Calculate viewport dimensions
	boxPadding := 6 // 3 on each side for border + padding
	boxWidth := helpWidth

	// Leave room for title, instructions, and padding
	availableHeight := termH - 6 // Room for instructions at bottom
	if availableHeight < 10 {
		availableHeight = 10
	}

	// Initialize or update help viewport
	if m.helpVp.Width == 0 {
		m.helpVp = viewport.New(helpWidth-boxPadding, availableHeight)
		m.helpVp.SetContent(helpContent)
	} else {
		m.helpVp.Width = helpWidth - boxPadding
		m.helpVp.Height = availableHeight
		m.helpVp.SetContent(helpContent)
	}

	// Create the box around viewport
	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(green).
		Padding(1, 2).
		Width(boxWidth)

	helpBox := boxStyle.Render(m.helpVp.View())

	// Center the help box horizontally
	centered := lipgloss.PlaceHorizontal(termW, lipgloss.Center, helpBox)

	// Close instruction with scroll indicator
	scrollInfo := ""
	if m.helpVp.TotalLineCount() > m.helpVp.Height {
		scrollPercentage := int(m.helpVp.ScrollPercent() * 100)
		scrollInfo = fmt.Sprintf(" (Scroll: %d%%) ", scrollPercentage)
	}

	closeInstruction := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#8fb9a8")).
		Align(lipgloss.Center).
		Render(fmt.Sprintf("↑/↓ or PgUp/PgDown to scroll%s • Press ? or Esc to close", scrollInfo))

	return "\n" + centered + "\n" + closeInstruction
}

func (m *model) renderContactBox(innerWidth int) string {
	const contactSlack = 4
	boxContentW := innerWidth - contactSlack

	if boxContentW < 20 {
		boxContentW = 20
	}

	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(green).
		MarginBottom(1)

	iconStyle := lipgloss.NewStyle().
		Foreground(teal).
		Bold(true)

	labelStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#8fb9a8"))

	valueStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#e8f4f0")).
		Background(lipgloss.Color("#0a2622")).
		Padding(0, 1)

	// Icon map for different contact types
	iconMap := map[string]string{
		"email":    "✉  ",
		"github":   "🐙 ",
		"linkedin": "💼 ",
		"website":  "🌐 ",
		"phone":    "📱 ",
		"twitter":  "🐦 ",
		"discord":  "💬 ",
	}

	// Parse the markdown content
	lines := strings.Split(strings.TrimSpace(m.contact), "\n")
	var styledLines []string

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Handle title (lines starting with #)
		if strings.HasPrefix(line, "#") {
			title := strings.TrimPrefix(line, "# ")
			title = strings.TrimPrefix(title, "## ")
			title = strings.TrimPrefix(title, "### ")
			styledLines = append(styledLines, titleStyle.Render("📬 "+title))
			styledLines = append(styledLines, "")
			continue
		}

		// Skip empty lines
		if line == "" {
			continue
		}

		// Handle "Label: Value" format
		if strings.Contains(line, ":") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				label := strings.TrimSpace(parts[0])
				value := strings.TrimSpace(parts[1])

				// Get icon based on label (case-insensitive)
				labelLower := strings.ToLower(label)
				icon := "• "
				for key, ico := range iconMap {
					if strings.Contains(labelLower, key) {
						icon = ico
						break
					}
				}

				styledLine := iconStyle.Render(icon) +
					labelStyle.Render(label+": ") +
					valueStyle.Render(value)

				styledLines = append(styledLines, styledLine)
				styledLines = append(styledLines, "")
			}
		}
	}

	// Remove trailing empty line if exists
	if len(styledLines) > 0 && styledLines[len(styledLines)-1] == "" {
		styledLines = styledLines[:len(styledLines)-1]
	}

	content := strings.Join(styledLines, "\n")

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#284d47")).
		Padding(1).
		Width(innerWidth).
		Render(content)
}

func parseProjects(md string) []Project {
	lines := strings.Split(md, "\n")
	var projects []Project
	var p Project

	for _, line := range lines {
		if strings.HasPrefix(line, "## ") {
			if p.Title != "" {
				projects = append(projects, p)
			}
			p = Project{Title: strings.TrimPrefix(line, "## ")}
		}
		if strings.HasPrefix(line, "Link: ") {
			p.Link = strings.TrimPrefix(line, "Link: ")
		}
		if strings.HasPrefix(line, "**Tech:**") {
			p.Tech = strings.TrimPrefix(line, "**Tech:** ")
		}
		if !strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "Link:") && !strings.HasPrefix(line, "**Tech:**") && strings.TrimSpace(line) != "" {
			if p.Description == "" {
				p.Description = line
			}
		}
	}
	if p.Title != "" {
		projects = append(projects, p)
	}
	return projects
}
