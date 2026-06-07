package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/xuri/excelize/v2"
)

const (
	profileDirName  = ".meli-reanalise-reclamacao"
	loginCheckURL   = "https://www.mercadolivre.com.br/vendas/omni/lista"
	chatPageURL     = "https://www.mercadolivre.com.br/metricas/meu-atendimento/resumo"
	loginTimeoutMin = 5
	batchSize       = 50
)

// claimResult holds the parsed result for each claim/sale number.
type claimResult struct {
	Number   string
	Category string // "Excluída", "Sem impacto na reputação", "Continua impactando"
}

func main() {
	log.SetFlags(log.Ltime)

	fmt.Println("╔══════════════════════════════════════════════════╗")
	fmt.Println("║  MELI - Reanálise de reclamações                ║")
	fmt.Println("╚══════════════════════════════════════════════════╝")
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)

	profileDir := getProfileDir()
	fmt.Printf("[INFO] Perfil do Chrome: %s\n", profileDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\n[INFO] Encerrando...")
		cancel()
	}()

	chromePath := findChrome()
	if chromePath != "" {
		fmt.Printf("[INFO] Chrome encontrado: %s\n", chromePath)
	}

	opts := buildAllocOpts(profileDir, chromePath)

	fmt.Println("[INFO] Iniciando o Chrome...")

	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, opts...)
	defer allocCancel()

	browserCtx, browserCancel := chromedp.NewContext(allocCtx,
		chromedp.WithLogf(log.Printf),
	)
	defer browserCancel()

	if err := ensureLogin(browserCtx); err != nil {
		log.Fatalf("[ERRO] Falha ao verificar login: %v", err)
	}

	// Persistent line reader for paste-friendly input
	lineCh := make(chan string, 1000)
	go func() {
		for {
			line, err := reader.ReadString('\n')
			lineCh <- strings.TrimSpace(line)
			if err != nil {
				close(lineCh)
				return
			}
		}
	}()

	for {
		inputs := readInputs(lineCh)
		if len(inputs) == 0 {
			fmt.Println("[INFO] Nenhum número fornecido. Encerrando.")
			break
		}

		// Clean and deduplicate inputs
		numbers := cleanInputs(inputs)
		total := len(numbers)
		fmt.Printf("\n[INFO] %d número(s) para processar.\n\n", total)

		if total == 0 {
			fmt.Println("[INFO] Nenhum número válido encontrado.")
			continue
		}

		// Process in batches of 50
		var allResults []claimResult
		batchCount := (total + batchSize - 1) / batchSize

		for b := 0; b < batchCount; b++ {
			select {
			case <-ctx.Done():
				fmt.Println("[INFO] Cancelado pelo usuário.")
				goto done
			default:
			}

			start := b * batchSize
			end := start + batchSize
			if end > total {
				end = total
			}
			batch := numbers[start:end]

			fmt.Printf("━━━ Lote %d/%d (%d números) ━━━\n", b+1, batchCount, len(batch))

			results, err := processBatch(browserCtx, batch, b == 0)
			if err != nil {
				fmt.Printf("[ERRO] Falha no lote %d: %v\n", b+1, err)
				// Mark all in this batch as error
				for _, num := range batch {
					allResults = append(allResults, claimResult{Number: num, Category: "Erro ao processar"})
				}
				continue
			}

			allResults = append(allResults, results...)
			fmt.Printf("[OK] Lote %d/%d processado.\n\n", b+1, batchCount)

			// Wait between batches
			if b < batchCount-1 {
				fmt.Println("[INFO] Aguardando 3 segundos antes do próximo lote...")
				time.Sleep(3 * time.Second)
			}
		}

	done:
		printSummary(allResults)
		excelPath := generateExcel(allResults)
		if excelPath != "" {
			fmt.Printf("[INFO] Relatório salvo em: %s\n", excelPath)
		}

		fmt.Println()
		fmt.Println("Cole mais números para continuar ou pressione Enter sem nada para sair.")
	}

	fmt.Println("[INFO] Programa encerrado.")
}

// processBatch handles a single batch of up to 50 numbers via the MELI chat.
func processBatch(ctx context.Context, numbers []string, isFirstBatch bool) ([]claimResult, error) {
	// Step 1: Navigate to the chat page
	fmt.Println("  Navegando para a página de métricas...")
	if err := chromedp.Run(ctx,
		chromedp.Navigate(chatPageURL),
		chromedp.WaitReady("body", chromedp.ByQuery),
	); err != nil {
		return nil, fmt.Errorf("navegar para métricas: %w", err)
	}
	time.Sleep(3 * time.Second)

	// Step 2: Open the assistant chat by clicking the floating button
	fmt.Println("  Abrindo o assistente...")

	// First try clicking the floating action button
	var chatOpened bool
	err := chromedp.Run(ctx,
		chromedp.Evaluate(`
			(function() {
				// Try to find and click the assistant button
				var btn = document.querySelector('button.action-button[data-component="WIDGET"]');
				if (btn) { btn.click(); return true; }
				// Try the top nav "Assistente" button
				btn = document.querySelector('a[href*="assistente"], button[aria-label*="assistente"], button[aria-label*="Assistente"]');
				if (btn) { btn.click(); return true; }
				// Try any button with "Assistente" text
				var buttons = document.querySelectorAll('button');
				for (var i = 0; i < buttons.length; i++) {
					if (buttons[i].textContent.includes('Assistente')) {
						buttons[i].click();
						return true;
					}
				}
				return false;
			})()
		`, &chatOpened),
	)
	if err != nil {
		return nil, fmt.Errorf("abrir assistente: %w", err)
	}

	if !chatOpened {
		// Try clicking the header "Assistente" link
		chromedp.Run(ctx, chromedp.Evaluate(`
			(function() {
				var links = document.querySelectorAll('*');
				for (var i = 0; i < links.length; i++) {
					if (links[i].textContent.trim() === 'Assistente' || links[i].textContent.trim() === '✦ Assistente') {
						links[i].click();
						return true;
					}
				}
				return false;
			})()
		`, &chatOpened))
	}

	// Wait for chat to load
	time.Sleep(3 * time.Second)

	// Step 3: Check if the chat input is available
	fmt.Println("  Verificando se o chat está aberto...")
	var chatInputExists bool
	for attempt := 0; attempt < 10; attempt++ {
		chromedp.Run(ctx, chromedp.Evaluate(`
			document.querySelector('#chat-input, textarea[aria-label*="chat"], textarea[placeholder*="assistente"], textarea[placeholder*="Pergunte"]') !== null
		`, &chatInputExists))
		if chatInputExists {
			break
		}
		time.Sleep(1 * time.Second)
	}

	if !chatInputExists {
		return nil, fmt.Errorf("chat não abriu - input não encontrado")
	}

	// Step 4: Start a new chat (click the "new chat" button if available)
	fmt.Println("  Iniciando nova conversa...")
	chromedp.Run(ctx, chromedp.Evaluate(`
		(function() {
			// Try to find "new chat" button (pencil icon at the top of chat)
			var btn = document.querySelector('.chat-header button, button[aria-label*="nova"], button[aria-label*="new"]');
			if (!btn) {
				// Look for SVG-based new chat button at the top left of chat panel
				var svgBtns = document.querySelectorAll('button svg, a svg');
				// The first icon in the chat header is typically "new conversation"
			}
			return false;
		})()
	`, nil))

	// Step 5: Send the prompt message
	prompt := "Remova as reclamações abaixo que estão impactando minha reputação e podem ser excluídas:"
	fmt.Println("  Enviando prompt...")

	if err := sendChatMessage(ctx, prompt); err != nil {
		return nil, fmt.Errorf("enviar prompt: %w", err)
	}

	// Wait for the assistant to respond (it will ask for the IDs)
	fmt.Println("  Aguardando resposta do assistente...")
	if err := waitForResponse(ctx, 30*time.Second); err != nil {
		return nil, fmt.Errorf("aguardar resposta do prompt: %w", err)
	}

	// Step 6: Send the numbers
	numbersText := strings.Join(numbers, "\n")
	fmt.Printf("  Enviando %d números...\n", len(numbers))

	if err := sendChatMessage(ctx, numbersText); err != nil {
		return nil, fmt.Errorf("enviar números: %w", err)
	}

	// Step 7: Wait for the response with the analysis
	fmt.Println("  Aguardando análise do MELI (pode levar até 60 segundos)...")
	if err := waitForResponse(ctx, 90*time.Second); err != nil {
		return nil, fmt.Errorf("aguardar análise: %w", err)
	}

	// Step 8: Extract the response text
	fmt.Println("  Coletando resposta...")
	responseText, err := getLastAssistantMessage(ctx)
	if err != nil {
		return nil, fmt.Errorf("coletar resposta: %w", err)
	}

	fmt.Printf("  Resposta do MELI:\n")
	// Print response indented
	for _, line := range strings.Split(responseText, "\n") {
		fmt.Printf("    %s\n", line)
	}
	fmt.Println()

	// Step 9: Parse the response
	results := parseResponse(responseText, numbers)
	return results, nil
}

// sendChatMessage types a message into the chat input and sends it.
func sendChatMessage(ctx context.Context, message string) error {
	// Focus the chat input
	if err := chromedp.Run(ctx, chromedp.Evaluate(`
		(function() {
			var input = document.querySelector('#chat-input, textarea[aria-label*="chat"], textarea[placeholder*="assistente"], textarea[placeholder*="Pergunte"]');
			if (input) {
				input.focus();
				input.value = '';
				// Trigger React's onChange
				var nativeInputValueSetter = Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype, 'value').set;
				nativeInputValueSetter.call(input, '');
				input.dispatchEvent(new Event('input', { bubbles: true }));
				return true;
			}
			return false;
		})()
	`, nil)); err != nil {
		return fmt.Errorf("focar input: %w", err)
	}

	time.Sleep(500 * time.Millisecond)

	// Set the message text using React-compatible value setter
	if err := chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(`
		(function() {
			var input = document.querySelector('#chat-input, textarea[aria-label*="chat"], textarea[placeholder*="assistente"], textarea[placeholder*="Pergunte"]');
			if (input) {
				input.focus();
				var nativeInputValueSetter = Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype, 'value').set;
				nativeInputValueSetter.call(input, %q);
				input.dispatchEvent(new Event('input', { bubbles: true }));
				// Adjust textarea height
				input.style.height = 'auto';
				input.style.height = input.scrollHeight + 'px';
				return true;
			}
			return false;
		})()
	`, message), nil)); err != nil {
		return fmt.Errorf("definir texto: %w", err)
	}

	time.Sleep(500 * time.Millisecond)

	// Click the send button
	if err := chromedp.Run(ctx, chromedp.Evaluate(`
		(function() {
			// Look for send button (arrow icon)
			var btns = document.querySelectorAll('button[type="button"]');
			for (var i = 0; i < btns.length; i++) {
				var svg = btns[i].querySelector('svg');
				if (svg && btns[i].closest('.new-chat-input__actions, .chat-input')) {
					// Check if it looks like a send button (not upload, camera, or audio)
					var ariaLabel = btns[i].getAttribute('aria-label') || '';
					if (ariaLabel.includes('nviar') || ariaLabel.includes('end')) {
						btns[i].click();
						return 'sent-aria';
					}
				}
			}
			// Try finding a submit-like button within the chat input area
			var sendBtn = document.querySelector('.new-chat-input button[type="submit"], .chat-input button[type="submit"]');
			if (sendBtn) {
				sendBtn.click();
				return 'sent-submit';
			}
			// Fallback: simulate Enter key on the input
			var input = document.querySelector('#chat-input');
			if (input) {
				input.dispatchEvent(new KeyboardEvent('keydown', {key: 'Enter', code: 'Enter', keyCode: 13, which: 13, bubbles: true}));
				input.dispatchEvent(new KeyboardEvent('keypress', {key: 'Enter', code: 'Enter', keyCode: 13, which: 13, bubbles: true}));
				input.dispatchEvent(new KeyboardEvent('keyup', {key: 'Enter', code: 'Enter', keyCode: 13, which: 13, bubbles: true}));
				return 'sent-enter';
			}
			return 'not-found';
		})()
	`, nil)); err != nil {
		return fmt.Errorf("enviar mensagem: %w", err)
	}

	time.Sleep(1 * time.Second)
	return nil
}

// waitForResponse waits until the assistant finishes responding (loading indicator disappears).
func waitForResponse(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	// First wait for loading to start (the dots appear)
	time.Sleep(2 * time.Second)

	// Then wait for loading to finish
	for time.Now().Before(deadline) {
		var isLoading bool
		chromedp.Run(ctx, chromedp.Evaluate(`
			(function() {
				// Check for typing indicator, loading dots, or pending message
				var loading = document.querySelector('.typing-indicator, .loading-dots, .message-item--loading, .chat-messages__loading');
				if (loading) return true;
				// Check if the last message is still being typed (streaming)
				var msgs = document.querySelectorAll('.message-item--assistant');
				if (msgs.length > 0) {
					var last = msgs[msgs.length - 1];
					// If it has a loading class or the content is empty
					if (last.querySelector('.typing-indicator, .loading-dots, .message-loading')) return true;
				}
				// Check for any animated dots (common loading pattern)
				var dots = document.querySelectorAll('[class*="loading"], [class*="typing"], [class*="dot-"]');
				for (var i = 0; i < dots.length; i++) {
					if (dots[i].closest('.chat-messages, .message-item')) return true;
				}
				return false;
			})()
		`, &isLoading))

		if !isLoading {
			// Double-check: wait a moment and verify it's really done
			time.Sleep(2 * time.Second)
			chromedp.Run(ctx, chromedp.Evaluate(`
				(function() {
					var loading = document.querySelector('.typing-indicator, .loading-dots, .message-item--loading');
					return loading !== null;
				})()
			`, &isLoading))
			if !isLoading {
				return nil
			}
		}
		time.Sleep(1 * time.Second)
	}

	return fmt.Errorf("timeout aguardando resposta do assistente")
}

// getLastAssistantMessage retrieves the text of the last assistant message.
func getLastAssistantMessage(ctx context.Context) (string, error) {
	var text string
	err := chromedp.Run(ctx, chromedp.Evaluate(`
		(function() {
			var msgs = document.querySelectorAll('.message-item--assistant');
			if (msgs.length === 0) return '';
			var last = msgs[msgs.length - 1];
			// Get text content, preserving some structure
			var content = last.querySelector('.chat-message__content');
			if (content) return content.textContent.trim();
			return last.textContent.trim();
		})()
	`, &text))
	if err != nil {
		return "", err
	}
	if text == "" {
		return "", fmt.Errorf("resposta vazia do assistente")
	}
	return text, nil
}

// parseResponse parses the assistant's response text and categorizes each number.
func parseResponse(response string, originalNumbers []string) []claimResult {
	responseLower := strings.ToLower(response)
	results := make([]claimResult, 0, len(originalNumbers))

	// Extract numbers from the response that appear in each category
	excludedNumbers := make(map[string]bool)
	noImpactNumbers := make(map[string]bool)
	impactingNumbers := make(map[string]bool)

	// Split response into sections
	lines := strings.Split(response, "\n")

	currentSection := ""
	for _, line := range lines {
		lineLower := strings.ToLower(strings.TrimSpace(line))

		// Detect section headers
		if strings.Contains(lineLower, "excluíd") || strings.Contains(lineLower, "excluid") ||
			strings.Contains(lineLower, "removid") {
			currentSection = "excluded"
		} else if strings.Contains(lineLower, "sem impacto") || strings.Contains(lineLower, "não afet") ||
			strings.Contains(lineLower, "não impacta") {
			currentSection = "no_impact"
		} else if strings.Contains(lineLower, "continuam impactando") || strings.Contains(lineLower, "seguem impactando") ||
			strings.Contains(lineLower, "não consegu") || strings.Contains(lineLower, "não foi possível") {
			currentSection = "impacting"
		}

		// Extract numbers from this line
		re := regexp.MustCompile(`\d{7,13}`)
		found := re.FindAllString(line, -1)
		for _, num := range found {
			switch currentSection {
			case "excluded":
				excludedNumbers[num] = true
			case "no_impact":
				noImpactNumbers[num] = true
			case "impacting":
				impactingNumbers[num] = true
			}
		}
	}

	// Also try to parse summary line like "Excluídas agora: 2" to validate
	// But individual number assignment is more important

	// Map results for each original number
	for _, num := range originalNumbers {
		category := "Não identificado na resposta"

		if excludedNumbers[num] {
			category = "Excluída"
		} else if noImpactNumbers[num] {
			category = "Sem impacto na reputação"
		} else if impactingNumbers[num] {
			category = "Continua impactando"
		} else {
			// Try harder: search in full response
			if strings.Contains(response, num) {
				// Number is mentioned but not clearly categorized
				// Use context around it
				idx := strings.Index(response, num)
				if idx >= 0 {
					surroundStart := idx - 200
					if surroundStart < 0 {
						surroundStart = 0
					}
					surroundEnd := idx + len(num) + 50
					if surroundEnd > len(response) {
						surroundEnd = len(response)
					}
					surrounding := strings.ToLower(response[surroundStart:surroundEnd])
					if strings.Contains(surrounding, "excluíd") || strings.Contains(surrounding, "removid") {
						category = "Excluída"
					} else if strings.Contains(surrounding, "sem impacto") || strings.Contains(surrounding, "não afet") {
						category = "Sem impacto na reputação"
					} else if strings.Contains(surrounding, "impactando") || strings.Contains(surrounding, "não consegu") {
						category = "Continua impactando"
					}
				}
			} else {
				// Number not found in response at all — check if response mentions
				// total counts that can help
				if strings.Contains(responseLower, "nenhum") && strings.Contains(responseLower, "aprovad") {
					category = "Continua impactando"
				}
			}
		}

		results = append(results, claimResult{Number: num, Category: category})
	}

	return results
}

// --- Input handling ---

func readInputs(lineCh <-chan string) []string {
	fmt.Println("Cole os números de venda ou reclamação abaixo.")
	fmt.Println("Após colar, aguarde 2 segundos para iniciar automaticamente,")
	fmt.Println("ou pressione Enter em uma linha vazia para iniciar.")
	fmt.Println()

	var inputs []string
	for {
		if len(inputs) == 0 {
			line, ok := <-lineCh
			if !ok {
				return inputs
			}
			if line == "" {
				return nil
			}
			inputs = append(inputs, line)
			fmt.Printf("  + Entrada adicionada (%d)\n", len(inputs))
		} else {
			select {
			case line, ok := <-lineCh:
				if !ok || line == "" {
					return inputs
				}
				inputs = append(inputs, line)
				fmt.Printf("  + Entrada adicionada (%d)\n", len(inputs))
			case <-time.After(2 * time.Second):
				fmt.Printf("  [AUTO] %d entrada(s) detectada(s). Iniciando...\n", len(inputs))
				return inputs
			}
		}
	}
}

func cleanInputs(inputs []string) []string {
	re := regexp.MustCompile(`\d{7,13}`)
	seen := make(map[string]bool)
	var result []string
	for _, input := range inputs {
		// Extract numbers from each line
		matches := re.FindAllString(input, -1)
		for _, m := range matches {
			if !seen[m] {
				seen[m] = true
				result = append(result, m)
			}
		}
	}
	return result
}

// --- Chrome setup ---

func buildAllocOpts(profileDir string, chromePath string) []chromedp.ExecAllocatorOption {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.UserDataDir(profileDir),
		chromedp.Flag("disable-gpu", false),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.Flag("disable-extensions", false),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("headless", false), // Always visible — user needs to see the chat
		chromedp.WindowSize(1280, 900),
	)
	if chromePath != "" {
		opts = append(opts, chromedp.ExecPath(chromePath))
	}
	return opts
}

func getProfileDir() string {
	var base string
	switch runtime.GOOS {
	case "windows":
		base = os.Getenv("LOCALAPPDATA")
		if base == "" {
			base = os.Getenv("USERPROFILE")
		}
	case "darwin":
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, "Library", "Application Support")
	default:
		home, _ := os.UserHomeDir()
		base = home
	}
	return filepath.Join(base, profileDirName)
}

func ensureLogin(ctx context.Context) error {
	fmt.Println("[INFO] Verificando login no Mercado Livre...")

	if err := chromedp.Run(ctx,
		chromedp.Navigate(loginCheckURL),
		chromedp.WaitReady("body", chromedp.ByQuery),
	); err != nil {
		return fmt.Errorf("navegar para ML: %w", err)
	}

	time.Sleep(3 * time.Second)

	var currentURL string
	if err := chromedp.Run(ctx, chromedp.Location(&currentURL)); err != nil {
		return fmt.Errorf("obter URL atual: %w", err)
	}

	if strings.Contains(currentURL, "login") || strings.Contains(currentURL, "registration") {
		fmt.Println()
		fmt.Println("╔══════════════════════════════════════════════════╗")
		fmt.Println("║  LOGIN NECESSÁRIO                                ║")
		fmt.Println("║                                                  ║")
		fmt.Println("║  O Chrome abriu a página de login do ML.         ║")
		fmt.Println("║  Faça login na janela do Chrome que abriu.       ║")
		fmt.Printf("║  Aguardando até %d minutos...                    ║\n", loginTimeoutMin)
		fmt.Println("╚══════════════════════════════════════════════════╝")
		fmt.Println()

		timeout := time.After(time.Duration(loginTimeoutMin) * time.Minute)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-timeout:
				return fmt.Errorf("timeout: login não realizado em %d minutos", loginTimeoutMin)
			case <-ticker.C:
				if err := chromedp.Run(ctx, chromedp.Location(&currentURL)); err != nil {
					continue
				}
				if !strings.Contains(currentURL, "login") && !strings.Contains(currentURL, "registration") {
					fmt.Println("[OK] Login detectado com sucesso!")
					time.Sleep(2 * time.Second)
					return nil
				}
			}
		}
	}

	fmt.Println("[OK] Já está logado no Mercado Livre!")
	return nil
}

func findChrome() string {
	candidates := []string{
		"/usr/bin/google-chrome-stable",
		"/usr/bin/google-chrome",
		"/usr/bin/chromium-browser",
		"/usr/bin/chromium",
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/opt/.devin/chrome/chrome/linux-133.0.6943.126/chrome-linux64/chrome",
		"/opt/.devin/chrome/chrome/linux-137.0.7118.2/chrome-linux64/chrome",
	}

	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// --- Summary & Excel ---

func printSummary(results []claimResult) {
	excluded := 0
	noImpact := 0
	impacting := 0
	errors := 0
	for _, r := range results {
		switch r.Category {
		case "Excluída":
			excluded++
		case "Sem impacto na reputação":
			noImpact++
		case "Continua impactando":
			impacting++
		default:
			errors++
		}
	}

	fmt.Println()
	fmt.Println("═══════════════════════════════════════")
	fmt.Printf("Total analisado: %d\n", len(results))
	fmt.Printf("Excluídas: %d\n", excluded)
	fmt.Printf("Continuam impactando: %d\n", impacting)
	fmt.Printf("Sem impacto na reputação: %d\n", noImpact)
	if errors > 0 {
		fmt.Printf("Não identificados: %d\n", errors)
	}
	fmt.Println("═══════════════════════════════════════")
}

func generateExcel(results []claimResult) string {
	if len(results) == 0 {
		return ""
	}

	f := excelize.NewFile()
	sheet := "Reanálise"
	f.SetSheetName("Sheet1", sheet)

	// Summary counts
	excluded := 0
	noImpact := 0
	impacting := 0
	for _, r := range results {
		switch r.Category {
		case "Excluída":
			excluded++
		case "Sem impacto na reputação":
			noImpact++
		case "Continua impactando":
			impacting++
		}
	}

	// Header style
	headerStyle, _ := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "FFFFFF", Size: 11},
		Fill:      excelize.Fill{Type: "pattern", Color: []string{"2D6AB1"}, Pattern: 1},
		Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center"},
		Border: []excelize.Border{
			{Type: "left", Color: "000000", Style: 1},
			{Type: "right", Color: "000000", Style: 1},
			{Type: "top", Color: "000000", Style: 1},
			{Type: "bottom", Color: "000000", Style: 1},
		},
	})

	// Summary style
	summaryStyle, _ := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Size: 11},
		Alignment: &excelize.Alignment{Horizontal: "left"},
	})

	// Data styles by category
	excludedStyle, _ := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Color: "008000"},
		Fill: excelize.Fill{Type: "pattern", Color: []string{"E8F5E9"}, Pattern: 1},
		Border: []excelize.Border{
			{Type: "left", Color: "000000", Style: 1},
			{Type: "right", Color: "000000", Style: 1},
			{Type: "top", Color: "000000", Style: 1},
			{Type: "bottom", Color: "000000", Style: 1},
		},
	})

	impactingStyle, _ := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Color: "CC0000"},
		Fill: excelize.Fill{Type: "pattern", Color: []string{"FFEBEE"}, Pattern: 1},
		Border: []excelize.Border{
			{Type: "left", Color: "000000", Style: 1},
			{Type: "right", Color: "000000", Style: 1},
			{Type: "top", Color: "000000", Style: 1},
			{Type: "bottom", Color: "000000", Style: 1},
		},
	})

	noImpactStyle, _ := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Color: "666666"},
		Fill: excelize.Fill{Type: "pattern", Color: []string{"F5F5F5"}, Pattern: 1},
		Border: []excelize.Border{
			{Type: "left", Color: "000000", Style: 1},
			{Type: "right", Color: "000000", Style: 1},
			{Type: "top", Color: "000000", Style: 1},
			{Type: "bottom", Color: "000000", Style: 1},
		},
	})

	// Summary section
	f.SetCellValue(sheet, "A1", "Resumo da Reanálise")
	f.SetCellStyle(sheet, "A1", "A1", summaryStyle)
	f.SetCellValue(sheet, "A2", "Total analisado:")
	f.SetCellValue(sheet, "B2", len(results))
	f.SetCellValue(sheet, "A3", "Excluídas:")
	f.SetCellValue(sheet, "B3", excluded)
	f.SetCellValue(sheet, "A4", "Continuam impactando:")
	f.SetCellValue(sheet, "B4", impacting)
	f.SetCellValue(sheet, "A5", "Sem impacto na reputação:")
	f.SetCellValue(sheet, "B5", noImpact)

	// Data headers
	row := 7
	headers := []string{"Número da operação", "Resultado"}
	for i, h := range headers {
		cell := fmt.Sprintf("%c%d", 'A'+i, row)
		f.SetCellValue(sheet, cell, h)
		f.SetCellStyle(sheet, cell, cell, headerStyle)
	}

	// Data rows
	for i, r := range results {
		dataRow := row + 1 + i
		cellA := fmt.Sprintf("A%d", dataRow)
		cellB := fmt.Sprintf("B%d", dataRow)
		f.SetCellValue(sheet, cellA, r.Number)
		f.SetCellValue(sheet, cellB, r.Category)

		// Apply style based on category
		switch r.Category {
		case "Excluída":
			f.SetCellStyle(sheet, cellA, cellB, excludedStyle)
		case "Continua impactando":
			f.SetCellStyle(sheet, cellA, cellB, impactingStyle)
		case "Sem impacto na reputação":
			f.SetCellStyle(sheet, cellA, cellB, noImpactStyle)
		}
	}

	// Adjust column widths
	f.SetColWidth(sheet, "A", "A", 25)
	f.SetColWidth(sheet, "B", "B", 35)

	// Save to executable directory
	exePath, err := os.Executable()
	if err != nil {
		exePath = "."
	}
	exeDir := filepath.Dir(exePath)

	now := time.Now()
	name := fmt.Sprintf("reanalise_reclamacao_%s.xlsx", now.Format("2006-01-02_15-04-05"))
	fullPath := filepath.Join(exeDir, name)

	if err := f.SaveAs(fullPath); err != nil {
		// Fallback: save in current directory
		fullPath = name
		if err := f.SaveAs(fullPath); err != nil {
			fmt.Printf("[ERRO] Falha ao salvar Excel: %v\n", err)
			return ""
		}
	}

	return fullPath
}
