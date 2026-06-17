# meli-reanalise-reclamacao

Aplicação em Go para solicitar a reanálise de reclamações no Mercado Livre via chat do assistente.

## Funcionalidade

- Envia reclamações/vendas em lotes de até 50 para o chat do assistente do MELI
- Aguarda o retorno de cada lote antes de enviar o próximo
- Gera planilha Excel com o resultado consolidado

## Compilar

```bash
# Linux/Mac
go build -o meli-reanalise-reclamacao .

# Windows
GOOS=windows GOARCH=amd64 go build -o meli-reanalise-reclamacao.exe .
```

## Usar

1. Execute o programa
2. Faça login no Mercado Livre na janela do Chrome que abrir (primeira execução)
3. Cole os números de venda ou reclamação no terminal
4. Aguarde o processamento e a geração da planilha

## Colunas da planilha

| Coluna | Descrição |
|--------|-----------|
| Número da operação | Número da venda/reclamação |
| Resultado | Excluída / Continua impactando / Sem impacto na reputação |

## Resumo

A planilha inclui um resumo com totais: analisados, excluídos, continuam impactando, sem impacto.
