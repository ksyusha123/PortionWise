package core

import (
	"math"
	"testing"
)

func TestFoodWeightRecalculation(t *testing.T) {
	f := Food{Name: "Рис", Items: []Item{{Name: "Рис", Grams: 200, Per100: Nutrition{Calories: 130, Protein: 2.7, Fat: 0.3, Carbs: 28}}}, Nutrition: Nutrition{Calories: 9999}}
	if e := f.Validate(); e != nil {
		t.Fatal(e)
	}
	if f.Nutrition.Calories != 260 || f.Nutrition.Carbs != 56 {
		t.Fatal("client total trusted instead of computing portion")
	}
	f.Items[0].Grams = math.NaN()
	if f.Validate() == nil {
		t.Fatal("NaN accepted")
	}
}
