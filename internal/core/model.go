package core

import (
	"errors"
	"math"
	"strings"
	"time"
)

type Nutrition struct {
	Calories float64 `json:"calories"`
	Protein  float64 `json:"protein"`
	Fat      float64 `json:"fat"`
	Carbs    float64 `json:"carbs"`
}

func (n Nutrition) Valid() bool {
	for _, v := range []float64{n.Calories, n.Protein, n.Fat, n.Carbs} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 100000 {
			return false
		}
	}
	return true
}
func (n Nutrition) Add(v Nutrition) Nutrition {
	return Nutrition{n.Calories + v.Calories, n.Protein + v.Protein, n.Fat + v.Fat, n.Carbs + v.Carbs}
}
func (n Nutrition) Sub(v Nutrition) Nutrition {
	return Nutrition{n.Calories - v.Calories, n.Protein - v.Protein, n.Fat - v.Fat, n.Carbs - v.Carbs}
}
func (n Nutrition) Scale(v float64) Nutrition {
	return Nutrition{n.Calories * v, n.Protein * v, n.Fat * v, n.Carbs * v}
}

type Item struct {
	Name   string    `json:"name"`
	Grams  float64   `json:"grams"`
	Per100 Nutrition `json:"per_100g"`
}
type Food struct {
	Name      string    `json:"name"`
	Items     []Item    `json:"items,omitempty"`
	Nutrition Nutrition `json:"nutrition"`
	Note      string    `json:"note,omitempty"`
}

func (f *Food) Validate() error {
	f.Name = strings.TrimSpace(f.Name)
	if f.Name == "" || len([]rune(f.Name)) > 200 || len(f.Items) > 30 || len(f.Note) > 4000 {
		return errors.New("некорректное название или состав блюда")
	}
	if len(f.Items) > 0 {
		f.Nutrition = Nutrition{}
		for i := range f.Items {
			v := &f.Items[i]
			v.Name = strings.TrimSpace(v.Name)
			if v.Name == "" || len([]rune(v.Name)) > 200 || math.IsNaN(v.Grams) || math.IsInf(v.Grams, 0) || v.Grams <= 0 || v.Grams > 10000 || !v.Per100.Valid() {
				return errors.New("проверьте массу и КБЖУ ингредиентов")
			}
			f.Nutrition = f.Nutrition.Add(v.Per100.Scale(v.Grams / 100))
		}
	}
	if !f.Nutrition.Valid() {
		return errors.New("КБЖУ должны быть конечными неотрицательными числами")
	}
	return nil
}

type User struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Timezone string     `json:"timezone"`
	Target   *Nutrition `json:"target,omitempty"`
}

func (u User) Location() *time.Location {
	l, e := time.LoadLocation(u.Timezone)
	if e != nil {
		return time.UTC
	}
	return l
}

type Analysis struct {
	ID        string `json:"id"`
	UserID    string `json:"user_id"`
	Day       string `json:"day"`
	Status    string `json:"status"`
	Food      *Food  `json:"food,omitempty"`
	Remaining int    `json:"remaining"`
	CreatedAt string `json:"created_at"`
}
type Meal struct {
	ID               string    `json:"id"`
	UserID           string    `json:"user_id"`
	EatenAt          string    `json:"eaten_at"`
	SourceAnalysisID string    `json:"source_analysis_id,omitempty"`
	RequestKey       string    `json:"request_key,omitempty"`
	Food             Food      `json:"food"`
	AnalysisSnapshot *Analysis `json:"analysis_snapshot,omitempty"`
}
type Day struct {
	Date              string     `json:"date"`
	Meals             []Meal     `json:"meals"`
	Total             Nutrition  `json:"total"`
	Target            *Nutrition `json:"target,omitempty"`
	Remaining         *Nutrition `json:"remaining,omitempty"`
	TargetUnavailable bool       `json:"target_unavailable,omitempty"`
}
