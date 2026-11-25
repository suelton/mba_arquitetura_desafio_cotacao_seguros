# Captura uma consulta PromQL do Prometheus local (query_range) e grava:
#   <saida>.json  - resposta crua da API (a fonte)
#   <saida>.png   - grafico de linhas desenhado a partir dessa resposta
#   <saida>.txt   - consulta, janela e passo (a legenda)
# Uso: .\capturar-grafico.ps1 -Consulta '<promql>' -Saida antes-p95 -JanelaSegundos 900 -Titulo 'p95 ...'
param(
  [Parameter(Mandatory)] [string] $Consulta,
  [Parameter(Mandatory)] [string] $Saida,
  [int] $JanelaSegundos = 900,
  [int] $Passo = 5,
  [string] $Titulo = '',
  [string] $Prometheus = 'http://localhost:9090',
  [double] $EscalaMax = 0,        # fixa o teto do eixo Y (para comparar dois graficos na mesma escala)
  [long] $FimUnix = 0,               # fim da janela (unix s); 0 = agora
  [string] $DoJson = ''           # redesenha a partir de um .json ja capturado, sem consultar o Prometheus
)
Add-Type -AssemblyName System.Drawing
$fim = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds(); if ($FimUnix -gt 0) { $fim = $FimUnix }; $ini = $fim - $JanelaSegundos
if ($DoJson) {
  $r = Get-Content $DoJson -Raw | ConvertFrom-Json
  $ini = [long]$r.data.result[0].values[0][0]; $fim = $ini + $JanelaSegundos
} else {
  $r = Invoke-RestMethod -Uri "$Prometheus/api/v1/query_range" -Method Post -Body @{ query = $Consulta; start = $ini; end = $fim; step = $Passo }
  $r | ConvertTo-Json -Depth 8 | Set-Content "$Saida.json" -Encoding utf8
}
if (-not $DoJson) { @("consulta: $Consulta", "janela: ultimos $JanelaSegundos s ($([DateTimeOffset]::FromUnixTimeSeconds($ini).ToString('u')) a $([DateTimeOffset]::FromUnixTimeSeconds($fim).ToString('u')))", "passo: ${Passo}s") | Set-Content "$Saida.txt" -Encoding utf8 }

$series = @($r.data.result)
$w = 1100; $h = 520; $ml = 70; $mr = 220; $mt = 50; $mb = 50
$bmp = New-Object System.Drawing.Bitmap $w, $h; $g = [System.Drawing.Graphics]::FromImage($bmp)
$g.SmoothingMode = 'AntiAlias'; $g.Clear([System.Drawing.Color]::White)
$f = New-Object System.Drawing.Font 'Segoe UI', 9; $fb = New-Object System.Drawing.Font 'Segoe UI', 11, ([System.Drawing.FontStyle]::Bold)
$g.DrawString($Titulo, $fb, [System.Drawing.Brushes]::Black, 10, 8)
$g.DrawString("janela: ultimos $($JanelaSegundos)s | passo ${Passo}s | $Consulta", $f, [System.Drawing.Brushes]::Gray, 10, 28)
$max = 0.0
foreach ($s in $series) { foreach ($v in $s.values) { $d = 0.0; if ([double]::TryParse($v[1], [Globalization.NumberStyles]::Float, [Globalization.CultureInfo]::InvariantCulture, [ref]$d) -and -not [double]::IsNaN($d) -and $d -gt $max) { $max = $d } } }
if ($max -le 0) { $max = 1 }; $max = [math]::Ceiling($max * 1.1 * 10) / 10; if ($EscalaMax -gt 0) { $max = $EscalaMax }
$pw = $w - $ml - $mr; $ph = $h - $mt - $mb
$axis = New-Object System.Drawing.Pen ([System.Drawing.Color]::LightGray)
for ($i = 0; $i -le 5; $i++) {
  $y = $mt + $ph - ($ph * $i / 5); $g.DrawLine($axis, $ml, $y, $ml + $pw, $y)
  $g.DrawString(('{0:0.##}' -f ($max * $i / 5)), $f, [System.Drawing.Brushes]::Black, 15, $y - 8)
}
for ($i = 0; $i -le 5; $i++) {
  $x = $ml + $pw * $i / 5; $t = [DateTimeOffset]::FromUnixTimeSeconds($ini + [int]($JanelaSegundos * $i / 5)).ToString('HH:mm:ss')
  $g.DrawString($t, $f, [System.Drawing.Brushes]::Black, $x - 25, $mt + $ph + 6)
}
$cores = 'RoyalBlue', 'Crimson', 'SeaGreen', 'DarkOrange', 'Purple', 'Teal'
$k = 0
foreach ($s in $series) {
  $cor = [System.Drawing.Color]::FromName($cores[$k % $cores.Count]); $pen = New-Object System.Drawing.Pen $cor, 2
  $rot = (($s.metric.PSObject.Properties | Where-Object { $_.Name -in 'partner','tenant_id','result' } | ForEach-Object { "$($_.Name)=$($_.Value)" }) -join ',')
  if (-not $rot) { $rot = 'serie' }
  $g.FillRectangle((New-Object System.Drawing.SolidBrush $cor), $ml + $pw + 15, $mt + 18 * $k, 12, 12)
  $g.DrawString($rot, $f, [System.Drawing.Brushes]::Black, $ml + $pw + 32, $mt + 18 * $k - 2)
  $prev = $null
  foreach ($v in $s.values) {
    $d = 0.0
    if (-not [double]::TryParse($v[1], [Globalization.NumberStyles]::Float, [Globalization.CultureInfo]::InvariantCulture, [ref]$d) -or [double]::IsNaN($d)) { $prev = $null; continue }
    $x = $ml + $pw * (([double]$v[0] - $ini) / $JanelaSegundos); $y = $mt + $ph - $ph * ($d / $max)
    if ($prev) { $g.DrawLine($pen, $prev[0], $prev[1], $x, $y) } else { $g.FillEllipse((New-Object System.Drawing.SolidBrush $cor), $x - 2, $y - 2, 4, 4) }
    $prev = @($x, $y)
  }
  $k++
}
$g.DrawRectangle($axis, $ml, $mt, $pw, $ph)
$bmp.Save((Join-Path (Get-Location) "$Saida.png"), [System.Drawing.Imaging.ImageFormat]::Png)
"series=$($series.Count) max=$max -> $Saida.png"





