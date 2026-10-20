// genicon 生成 Firefly 应用图标（512×512 PNG，仅依赖标准库）。
// 用法：go run ./scripts/genicon -out build/icon.png
package main

import (
	"flag"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

func main() {
	out := flag.String("out", "build/icon.png", "输出 PNG 路径")
	flag.Parse()

	const S = 512
	img := image.NewRGBA(image.Rect(0, 0, S, S))

	// 圆角方形背景（半径 96），纵向渐变 #0e1116 → #1a2029
	radius := 96.0
	for y := 0; y < S; y++ {
		for x := 0; x < S; x++ {
			if !inRoundedRect(float64(x)+0.5, float64(y)+0.5, S, S, radius) {
				continue // 圆角外透明
			}
			t := float64(y) / S
			img.Set(x, y, color.RGBA{
				R: lerp(0x0e, 0x1a, t),
				G: lerp(0x11, 0x20, t),
				B: lerp(0x16, 0x29, t),
				A: 255,
			})
		}
	}

	// 萤火虫发光腹部：中心 (256, 320)，径向渐变 琥珀黄 → 黄绿 → 透明
	glowCx, glowCy, glowR := 256.0, 320.0, 190.0
	blendRadial(img, glowCx, glowCy, glowR, func(d float64) color.RGBA {
		a := math.Pow(1-d, 2.2) // 平滑衰减
		if d < 0.18 {
			// 核心亮斑
			return color.RGBA{255, 244, 180, uint8(255 * a)}
		}
		return color.RGBA{
			R: uint8(lerp(0xd4, 0x7a, d)),
			G: uint8(lerp(0xf0, 0xc7, d)),
			B: uint8(lerp(0x66, 0x4f, d)),
			A: uint8(255 * a),
		}
	})

	// 身体：深灰椭圆（头胸），中心 (256, 190)
	fillEllipse(img, 256, 190, 46, 92, color.RGBA{0x2a, 0x2f, 0x38, 255})
	// 头部
	fillEllipse(img, 256, 96, 34, 30, color.RGBA{0x22, 0x26, 0x2e, 255})
	// 触角
	stroke(img, 240, 78, 200, 40, 5, color.RGBA{0x22, 0x26, 0x2e, 255})
	stroke(img, 272, 78, 312, 40, 5, color.RGBA{0x22, 0x26, 0x2e, 255})
	// 双翅：半透明浅色椭圆
	fillEllipse(img, 190, 190, 40, 78, color.RGBA{0x8a, 0x94, 0xa6, 110})
	fillEllipse(img, 322, 190, 40, 78, color.RGBA{0x8a, 0x94, 0xa6, 110})

	// 环境光点（萤火虫氛围）
	dots := [][2]float64{{90, 120}, {420, 100}, {70, 400}, {440, 380}, {150, 60}, {380, 450}}
	for _, d := range dots {
		blendRadial(img, d[0], d[1], 14, func(k float64) color.RGBA {
			return color.RGBA{0xff, 0xe8, 0x9a, uint8(160 * math.Pow(1-k, 3))}
		})
	}

	f, err := os.Create(*out)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		panic(err)
	}
}

func lerp(a, b, t float64) uint8 {
	return uint8(math.Round(a + (b-a)*t))
}

func inRoundedRect(x, y, w, h, r float64) bool {
	// 四角圆弧判定
	cx, cy := x, y
	if x < r {
		cx = r
	} else if x > w-r {
		cx = w - r
	}
	if y < r {
		cy = r
	} else if y > h-r {
		cy = h - r
	}
	dx, dy := x-cx, y-cy
	if (cx == r || cx == w-r) && (cy == r || cy == h-r) {
		return dx*dx+dy*dy <= r*r
	}
	return x >= 0 && x <= w && y >= 0 && y <= h
}

// blendRadial 以 (cx,cy) 为心、r 为半径做径向颜色混合（colorFn 入参为归一化距离 0..1）。
func blendRadial(img *image.RGBA, cx, cy, r float64, colorFn func(d float64) color.RGBA) {
	x0 := int(math.Max(0, cx-r))
	x1 := int(math.Min(float64(img.Bounds().Max.X-1), cx+r))
	y0 := int(math.Max(0, cy-r))
	y1 := int(math.Min(float64(img.Bounds().Max.Y-1), cy+r))
	for y := y0; y <= y1; y++ {
		for x := x0; x <= x1; x++ {
			dx, dy := float64(x)+0.5-cx, float64(y)+0.5-cy
			d := math.Sqrt(dx*dx+dy*dy) / r
			if d > 1 {
				continue
			}
			src := colorFn(d)
			if src.A == 0 {
				continue
			}
			dst := img.RGBAAt(x, y)
			img.SetRGBA(x, y, over(src, dst))
		}
	}
}

// over 是标准 source-over alpha 合成。
func over(src, dst color.RGBA) color.RGBA {
	if src.A == 255 {
		return src
	}
	a := float64(src.A) / 255
	ia := 1 - a
	return color.RGBA{
		R: uint8(math.Round(float64(src.R)*a + float64(dst.R)*ia)),
		G: uint8(math.Round(float64(src.G)*a + float64(dst.G)*ia)),
		B: uint8(math.Round(float64(src.B)*a + float64(dst.B)*ia)),
		A: uint8(math.Round(float64(src.A) + float64(dst.A)*ia)),
	}
}

func fillEllipse(img *image.RGBA, cx, cy, rx, ry int, c color.RGBA) {
	for y := cy - ry; y <= cy+ry; y++ {
		for x := cx - rx; x <= cx+rx; x++ {
			dx, dy := float64(x-cx)/float64(rx), float64(y-cy)/float64(ry)
			if dx*dx+dy*dy <= 1 && img.Bounds().Dx() > x && x >= 0 && y >= 0 && img.Bounds().Dy() > y {
				img.SetRGBA(x, y, over(c, img.RGBAAt(x, y)))
			}
		}
	}
}

func stroke(img *image.RGBA, x0, y0, x1, y1, w int, c color.RGBA) {
	steps := int(math.Max(math.Abs(float64(x1-x0)), math.Abs(float64(y1-y0)))) * 2
	for i := 0; i <= steps; i++ {
		t := float64(i) / float64(steps)
		x := int(math.Round(float64(x0) + (float64(x1)-float64(x0))*t))
		y := int(math.Round(float64(y0) + (float64(y1)-float64(y0))*t))
		fillEllipse(img, x, y, w/2, w/2, c)
	}
}
