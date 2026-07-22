package main

import "testing"

func categories(results []claimResult) map[string]string {
	m := make(map[string]string, len(results))
	for _, r := range results {
		m[r.Number] = r.Category
	}
	return m
}

// Batch 1: assistant reported none excluded ("Pedidos sem exclusão: ...").
func TestParseResponseNoneExcluded(t *testing.T) {
	numbers := []string{
		"2000016588152178", "2000016587931308", "2000016587121768", "2000016587044846",
		"2000016586813090", "2000016586279600", "2000016585728976", "2000016585288250",
		"2000016585261392", "2000016585249624",
	}
	resp := "Revisei os 10 pedidos e, neste momento, nenhuma dessas reclamações pôde ser excluída do impacto na sua reputação.Pedidos sem exclusão: 2000016588152178, 2000016587931308, 2000016587121768, 2000016587044846, 2000016586813090, 2000016586279600, 2000016585728976, 2000016585288250, 2000016585261392 e 2000016585249624.Motivos principais: falta de entrega, atraso com envio sob responsabilidade do vendedor, ou produto com defeito/não funcionando.Resultado: o impacto dessas reclamações continua valendo na reputação."

	got := categories(parseResponse(resp, numbers))
	for _, n := range numbers {
		if got[n] != "Continua impactando" {
			t.Errorf("número %s: esperado \"Continua impactando\", obtido %q", n, got[n])
		}
	}
}

// Batch 2: assistant excluded exactly one order and listed the rest as not excluded.
func TestParseResponseOneExcluded(t *testing.T) {
	numbers := []string{
		"2000016585249480", "2000016585082008", "2000016584865668", "2000016584862414",
		"2000016584633594", "2000016584544720", "2000016584416798", "2000016584369404",
		"2000016584295434", "2000016584260508",
	}
	resp := "Revisei os 10 casos e 1 reclamação já foi excluída do impacto na sua reputação.Excluída: pedido 2000016584295434.Não excluídas: 2000016585249480, 2000016585082008, 2000016584865668, 2000016584862414, 2000016584633594, 2000016584544720, 2000016584416798, 2000016584369404 e 2000016584260508.Nos demais casos, a exclusão não foi aprovada porque as reclamações ficaram vinculadas a defeito, produto diferente, dano ou entrega sem evidência suficiente para retirar o impacto."

	got := categories(parseResponse(resp, numbers))
	if got["2000016584295434"] != "Excluída" {
		t.Errorf("2000016584295434: esperado \"Excluída\", obtido %q", got["2000016584295434"])
	}
	for _, n := range numbers {
		if n == "2000016584295434" {
			continue
		}
		if got[n] != "Continua impactando" {
			t.Errorf("número %s: esperado \"Continua impactando\", obtido %q", n, got[n])
		}
	}
}
