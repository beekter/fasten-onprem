//go:build exclude

//go:generate go run generate.go
package main

import (
	"encoding/json"
	"fmt"
	"github.com/dave/jennifer/jen"
	"github.com/iancoleman/strcase"
	"golang.org/x/exp/slices"
	"io/ioutil"
	"log"
	"sort"
	"strings"
)

type SearchParameter struct {
	Id           string   `json:"id"`
	Name         string   `json:"name"`
	Status       string   `json:"status"`
	Experimental bool     `json:"experimental"`
	Description  string   `json:"description"`
	Code         string   `json:"code"`
	Base         []string `json:"base"`
	Type         string   `json:"type"`
	XpathUsage   string   `json:"xpathUsage"`
	Expression   string   `json:"expression"`
	Target       []string `json:"target"`
}

type SearchParameterBundle struct {
	Entry []struct {
		Resource SearchParameter `json:"resource"`
	} `json:"entry"`
}

type DBField struct {
	FieldType          string
	Description        string
	FHIRPathExpression string
}

var licenseComment = strings.Split(strings.Trim(`
THIS FILE IS GENERATED BY https://github.com/fastenhealth/fasten-onprem/blob/main/backend/pkg/models/database/generate.go
PLEASE DO NOT EDIT BY HAND
`, "\n"), "\n")

func main() {
	// Read the search-parameters.json file
	searchParamsData, err := ioutil.ReadFile("search-parameters.json")
	if err != nil {
		log.Fatal(err)
	}

	// Parse the search-parameters.json file
	var searchParamsBundle SearchParameterBundle
	err = json.Unmarshal(searchParamsData, &searchParamsBundle)
	if err != nil {
		log.Fatal(err)
	}

	resourceFieldMap := map[string]map[string]DBField{}

	// Generate Go structs for each resource type
	for _, entry := range searchParamsBundle.Entry {
		if entry.Resource.Status != "active" && entry.Resource.Status != "draft" {
			continue
		}
		if entry.Resource.Type == "composite" || entry.Resource.Type == "special" {
			continue
		}
		if entry.Resource.Name == "patient" {
			//skip Patient, not needed for searching.
			continue
		}

		camelCaseResourceName := strcase.ToCamel(entry.Resource.Name)

		//log.Printf("processing %v", entry.Resource.Id)
		for _, resourceName := range entry.Resource.Base {

			if !slices.Contains(AllowedResources, resourceName) {
				continue
			}

			fieldMap, ok := resourceFieldMap[resourceName]
			if !ok {
				fieldMap = map[string]DBField{}
			}

			fieldMap[camelCaseResourceName] = DBField{
				FieldType:          entry.Resource.Type,
				Description:        entry.Resource.Description,
				FHIRPathExpression: entry.Resource.Expression,
			}

			resourceFieldMap[resourceName] = fieldMap
		}
	}
	// make sure all "base" resources have a field map
	for _, resourceName := range AllowedResources {
		_, ok := resourceFieldMap[resourceName]
		if !ok {
			resourceFieldMap[resourceName] = map[string]DBField{}
		}
	}

	//add default fields to all resources
	for resourceName, fieldMap := range resourceFieldMap {
		fieldMap["LastUpdated"] = DBField{
			FieldType:          "date",
			Description:        "When the resource version last changed",
			FHIRPathExpression: "Resource.meta.lastUpdated",
		}
		fieldMap["Language"] = DBField{
			FieldType:          "token",
			Description:        "Language of the resource content",
			FHIRPathExpression: "Resource.language",
		}
		fieldMap["Profile"] = DBField{
			FieldType:          "reference",
			Description:        "Profiles this resource claims to conform to",
			FHIRPathExpression: "Resource.meta.profile",
		}
		fieldMap["SourceUri"] = DBField{
			FieldType:          "uri",
			Description:        "Identifies where the resource comes from",
			FHIRPathExpression: "Resource.meta.source",
		}
		fieldMap["Tag"] = DBField{
			FieldType:          "token",
			Description:        "Tags applied to this resource",
			FHIRPathExpression: "Resource.meta.tag",
		}
		fieldMap["Text"] = DBField{
			FieldType:   "string",
			Description: "Text search against the narrative",
		}
		fieldMap["Type"] = DBField{
			FieldType:   "special",
			Description: "A resource type filter",
		}

		resourceFieldMap[resourceName] = fieldMap
	}

	// create files for each resource type
	for resourceName, fieldMap := range resourceFieldMap {

		file := jen.NewFile("database")
		for _, line := range licenseComment {
			file.HeaderComment(line)
		}

		// Generate fields for search parameters. Make sure they are in a sorted order, otherwise the generated code will be different each time.
		keys := make([]string, 0, len(fieldMap))
		for k, _ := range fieldMap {
			keys = append(keys, k)
		}

		// Generate struct declaration
		structName := "Fhir" + strings.Title(resourceName)
		file.Type().Id(structName).StructFunc(func(g *jen.Group) {
			//Add the OriginBase embedded struct
			g.Qual("github.com/fastenhealth/fastenhealth-onprem/backend/pkg/models", "ResourceBase")

			sort.Strings(keys)
			for _, fieldName := range keys {
				fieldInfo := fieldMap[fieldName]

				g.Comment(fieldInfo.Description)
				g.Comment(fmt.Sprintf("https://hl7.org/fhir/r4/search.html#%s", fieldInfo.FieldType))
				golangFieldType := mapFieldType(fieldInfo.FieldType)
				var golangFieldStatement *jen.Statement
				if strings.Contains(golangFieldType, "#") {
					golangFieldTypeParts := strings.Split(golangFieldType, "#")
					golangFieldStatement = g.Id(fieldName).Add(jen.Qual(golangFieldTypeParts[0], golangFieldTypeParts[1]))
				} else {
					golangFieldStatement = g.Id(fieldName).Add(jen.Id(golangFieldType))
				}
				golangFieldStatement.Tag(map[string]string{
					"json": fmt.Sprintf("%s,omitempty", strcase.ToLowerCamel(fieldName)),
					"gorm": fmt.Sprintf("column:%s;%s", strcase.ToLowerCamel(fieldName), mapGormType(fieldInfo.FieldType)),
				})
			}
		})

		//create an instance function that returns a map of all fields and their types
		file.Func().Call(jen.Id("s").Op("*").Id(structName)).Id("GetSearchParameters").Params().Params(jen.Map(jen.String()).String()).BlockFunc(func(g *jen.Group) {
			g.Id("searchParameters").Op(":=").Map(jen.String()).String().Values(jen.DictFunc(func(d jen.Dict) {
				for _, fieldName := range keys {
					fieldInfo := fieldMap[fieldName]
					fieldNameLowerCamel := strcase.ToLowerCamel(fieldName)
					d[jen.Lit(fieldNameLowerCamel)] = jen.Lit(fieldInfo.FieldType)
				}
			}))
			g.Return(jen.Id("searchParameters"))
		})

		//create an instance function that extracts all search parameters from the raw resource and populates the struct
		file.Func().Call(jen.Id("s").Op("*").Id(structName)).Id("PopulateAndExtractSearchParameters").Params(jen.Id("resourceRaw").Qual("encoding/json", "RawMessage")).Params(jen.Error()).BlockFunc(func(g *jen.Group) {
			//set resourceRaw to ResourceRaw field
			g.Id("s.ResourceRaw").Op("=").Qual("gorm.io/datatypes", "JSON").Call(jen.Id("resourceRaw"))

			g.Comment("unmarshal the raw resource (bytes) into a map")
			g.Var().Id("resourceRawMap").Map(jen.String()).Interface()
			g.Err().Op(":=").Qual("encoding/json", "Unmarshal").Call(jen.Id("resourceRaw"), jen.Op("&").Id("resourceRawMap"))
			g.If(jen.Err().Op("!=").Nil()).BlockFunc(func(g *jen.Group) {
				g.Return(jen.Err())
			})

			//check length of fhirPathJs script (may not have been embedded correctly)
			g.If(jen.Len(jen.Id("fhirPathJs")).Op("==").Lit(0)).BlockFunc(func(f *jen.Group) {
				f.Return(jen.Qual("fmt", "Errorf").Call(jen.Lit("fhirPathJs script is empty")))
			})

			//initialize goja vm
			g.Id("vm").Op(":=").Qual("github.com/dop251/goja", "New").Call()
			g.Comment("setup the global window object")
			g.Id("vm").Dot("Set").Call(jen.Lit("window"), jen.Id("vm").Dot("NewObject").Call())

			g.Comment("set the global FHIR Resource object")
			g.Id("vm").Dot("Set").Call(jen.Lit("fhirResource"), jen.Id("resourceRawMap"))

			g.Comment("compile the fhirpath library")
			g.List(jen.Id("fhirPathJsProgram"), jen.Id("err")).Op(":=").Qual("github.com/dop251/goja", "Compile").Call(jen.Lit("fhirpath.min.js"), jen.Id("fhirPathJs"), jen.True())
			g.If(jen.Err().Op("!=").Nil()).BlockFunc(func(e *jen.Group) {
				e.Return(jen.Err())
			})

			g.Comment("add the fhirpath library in the goja vm")
			g.List(jen.Id("_"), jen.Id("err")).Op("=").Id("vm").Dot("RunProgram").Call(jen.Id("fhirPathJsProgram"))
			g.If(jen.Err().Op("!=").Nil()).BlockFunc(func(e *jen.Group) {
				e.Return(jen.Err())
			})

			g.Comment("execute the fhirpath expression for each search parameter")
			for _, fieldName := range keys {
				fieldInfo := fieldMap[fieldName]
				//skip any empty fhirpath expressions, we cant extract anything
				if len(fieldInfo.FHIRPathExpression) == 0 {
					continue
				} else {
					//TODO: "Observation.value as CodeableConcept" and other similar expressions do not work with goja in Golang, but do work in Javascript
					// we're unsure why, but we can work around this by removing the " as " part of the expression, and instead use the fully qualified field name:
					// "Observation.valueCodeableConcept" instead of "Observation.value as CodeableConcept"
					// however, we cannot just remove the " As "string, as there are primitive types that start with a lowercase letter, and we need to uppercase the first letter
					// https://www.hl7.org/fhir/R4/datatypes.html#CodeableConcept
					// https://hl7.org/fhir/r4/formats.html#choice
					// this is a very naive implementation, but it works for now
					fieldInfo.FHIRPathExpression = strings.ReplaceAll(fieldInfo.FHIRPathExpression, " as string", " as String")
					fieldInfo.FHIRPathExpression = strings.ReplaceAll(fieldInfo.FHIRPathExpression, " as time", " as Time")
					fieldInfo.FHIRPathExpression = strings.ReplaceAll(fieldInfo.FHIRPathExpression, " as date", " as Date")
					fieldInfo.FHIRPathExpression = strings.ReplaceAll(fieldInfo.FHIRPathExpression, " as boolean", " as Boolean")
					fieldInfo.FHIRPathExpression = strings.ReplaceAll(fieldInfo.FHIRPathExpression, " as url", " as Url")
					fieldInfo.FHIRPathExpression = strings.ReplaceAll(fieldInfo.FHIRPathExpression, " as code", " as Code")
					fieldInfo.FHIRPathExpression = strings.ReplaceAll(fieldInfo.FHIRPathExpression, " as integer", " as Integer")
					fieldInfo.FHIRPathExpression = strings.ReplaceAll(fieldInfo.FHIRPathExpression, " as uri", " as Uri")
					fieldInfo.FHIRPathExpression = strings.ReplaceAll(fieldInfo.FHIRPathExpression, " as decimal", " as Decimal")

					//remove all " as " from the fhirpath expression, this does not work correctly with goja or otto
					fieldInfo.FHIRPathExpression = strings.ReplaceAll(fieldInfo.FHIRPathExpression, " as ", "")
				}

				g.Comment(fmt.Sprintf("extracting %s", fieldName))
				fieldNameVar := fmt.Sprintf("%sResult", strcase.ToLowerCamel(fieldName))
				g.List(jen.Id(fieldNameVar), jen.Id("err")).Op(":=").Id("vm").Dot("RunString").CallFunc(func(r *jen.Group) {

					script := fmt.Sprintf("window.fhirpath.evaluate(fhirResource, '%s')", fieldInfo.FHIRPathExpression)

					if fieldInfo.FieldType == "string" {
						//strings are unusual in that they can contain HumanName and Address types, which are not actually simple types
						//we need to do some additional processing,
						r.Op("`").Id(fmt.Sprintf(`
							%sResult = %s
							%sProcessed = %sResult.reduce((accumulator, currentValue) => {
								if (typeof currentValue === 'string') {
									//basic string
									accumulator.push(currentValue)
								} else if (currentValue.family  || currentValue.given) {
									//HumanName http://hl7.org/fhir/R4/datatypes.html#HumanName
									var humanNameParts = []
									if (currentValue.prefix) {
										humanNameParts = humanNameParts.concat(currentValue.prefix)
									}
									if (currentValue.given) {	
										humanNameParts = humanNameParts.concat(currentValue.given)
									}	
									if (currentValue.family) {	
										humanNameParts.push(currentValue.family)	
									}	
									if (currentValue.suffix) {	
										humanNameParts = humanNameParts.concat(currentValue.suffix)	
									}
									accumulator.push(humanNameParts.join(" "))
								} else if (currentValue.city || currentValue.state || currentValue.country || currentValue.postalCode) {
									//Address http://hl7.org/fhir/R4/datatypes.html#Address
									var addressParts = []		
									if (currentValue.line) {
										addressParts = addressParts.concat(currentValue.line)
									}
									if (currentValue.city) {
										addressParts.push(currentValue.city)
									}	
									if (currentValue.state) {	
										addressParts.push(currentValue.state)
									}	
									if (currentValue.postalCode) {
										addressParts.push(currentValue.postalCode)
									}	
									if (currentValue.country) {
										addressParts.push(currentValue.country)	
									}	
									accumulator.push(addressParts.join(" "))
								} else {
									//string, boolean
									accumulator.push(currentValue)
								}
								return accumulator
							}, [])
						
				
							JSON.stringify(%sProcessed)
						`, fieldName, script, fieldName, fieldName, fieldName)).Op("`")

					} else if isSimpleFieldType(fieldInfo.FieldType) {
						//TODO: we may end up losing some information here, as we are only returning the first element of the array
						script += "[0]"
						//"Don't JSON.stringfy simple types"
						r.Lit(script)
					} else if fieldInfo.FieldType == "token" {
						r.Op("`").Id(fmt.Sprintf(`
							%sResult = %s
							%sProcessed = %sResult.reduce((accumulator, currentValue) => {
								if (currentValue.coding) {
									//CodeableConcept
									currentValue.coding.map((coding) => {
										accumulator.push({
											"code": coding.code,	
											"system": coding.system,
											"text": currentValue.text
										})
									})
								} else if (currentValue.value) {
									//ContactPoint, Identifier
									accumulator.push({
										"code": currentValue.value,
										"system": currentValue.system,
										"text": currentValue.type?.text
									})
								} else if (currentValue.code) {
									//Coding
									accumulator.push({
										"code": currentValue.code,
										"system": currentValue.system,
										"text": currentValue.display
									})
								} else if ((typeof currentValue === 'string') || (typeof currentValue === 'boolean')) {
									//string, boolean
									accumulator.push({
										"code": currentValue,
									})
								}
								return accumulator
							}, [])
						
				
							JSON.stringify(%sProcessed)
						`, fieldName, script, fieldName, fieldName, fieldName)).Op("`")
					} else {
						r.Lit(fmt.Sprintf(`JSON.stringify(%s)`, script))
					}
				})
				g.If(jen.Err().Op("==").Nil().Op("&&").Id(fieldNameVar).Dot("String").Call().Op("!=").Lit("undefined")).BlockFunc(func(i *jen.Group) {
					switch fieldInfo.FieldType {
					case "token", "reference", "special", "quantity", "string":
						i.Id("s").Dot(fieldName).Op("=").Index().Byte().Parens(jen.Id(fieldNameVar).Dot("String").Call())
						break
					case "number":
						i.Id("s").Dot(fieldName).Op("=").Id(fieldNameVar).Dot("ToFloat").Call()
						break
					case "date":
						//parse RFC3339 date
						i.List(jen.Id("t"), jen.Id("err")).Op(":=").Qual("time", "Parse").Call(jen.Qual("time", "RFC3339"), jen.Id(fieldNameVar).Dot("String").Call())
						i.If(jen.Err().Op("==").Nil()).BlockFunc(func(e *jen.Group) {
							e.Id("s").Dot(fieldName).Op("=").Id("t")
						})
					case "uri":
						i.Id("s").Dot(fieldName).Op("=").Id(fieldNameVar).Dot("String").Call()
						break
					default:
						i.Id("s").Dot(fieldName).Op("=").Id(fieldNameVar).Dot("String").Call()
						break
					}

				})

			}
			g.Return(jen.Nil())

		})

		file.Comment("TableName overrides the table name from fhir_observations (pluralized) to `fhir_observation`. https://gorm.io/docs/conventions.html#TableName")
		file.Func().Call(jen.Id("s").Op("*").Id(structName)).Id("TableName").Params().Params(jen.String()).BlockFunc(func(g *jen.Group) {
			g.Return(jen.Lit(strcase.ToSnake(structName)))
		})

		// Save the generated Go code to a file
		filename := fmt.Sprintf("%s.go", strcase.ToSnake(structName))
		fmt.Printf("Generated Go struct for %s: %s\n", structName, filename)
		err = file.Save(filename)
		if err != nil {
			log.Fatal(err)
		}

	}

	bytes, err := json.MarshalIndent(resourceFieldMap["Observation"], "", "    ")
	log.Printf("%s, %v", string(bytes), err)

	utilsFile := jen.NewFile("database")

	// Generate go embed code for the fhirpath.js file
	//utilsFile.ImportName("embed", "")
	utilsFile.Anon("embed")
	utilsFile.Comment("//go:embed fhirpath.min.js")
	utilsFile.Var().Id("fhirPathJs").String()

	utilsFile.Comment("Generates all tables in the database associated with these models")
	utilsFile.Func().Id("Migrate").Params(
		jen.Id("gormClient").Op("*").Qual("gorm.io/gorm", "DB"),
	).Params(jen.Error()).BlockFunc(func(g *jen.Group) {

		/*
			err := sr.GormClient.AutoMigrate(
					&models.User{},
					&models.SourceCredential{},
					&models.ResourceFhir{},
					&models.Glossary{},
				)
				if err != nil {
					return fmt.Errorf("Failed to automigrate! - %v", err)
				}
				return nil
		*/
		g.Id("err").Op(":=").Id("gormClient").Dot("AutoMigrate").CallFunc(func(g *jen.Group) {
			for _, resourceName := range AllowedResources {
				g.Op("&").Id("Fhir" + resourceName).Values()
			}
		})

		g.If(jen.Id("err").Op("!=").Nil()).Values(jen.Return(jen.Id("err")))
		g.Return(jen.Nil())
	})

	//A function which returns a the corresponding FhirResource when provided the FhirResource type string
	//uses a switch statement to return the correct type
	utilsFile.Comment("Returns a map of all the resource names to their corresponding go struct")
	utilsFile.Func().Id("NewFhirResourceModelByType").Params(jen.Id("resourceType").String()).Params(jen.Id("IFhirResourceModel"), jen.Error()).BlockFunc(func(g *jen.Group) {
		g.Switch(jen.Id("resourceType")).BlockFunc(func(s *jen.Group) {
			for _, resourceName := range AllowedResources {
				s.Case(jen.Lit(resourceName)).BlockFunc(func(c *jen.Group) {
					c.Return(jen.Op("&").Id("Fhir"+resourceName).Values(), jen.Nil())
				})
			}
			s.Default().BlockFunc(func(d *jen.Group) {
				d.Return(jen.Nil(), jen.Qual("fmt", "Errorf").Call(jen.Lit("Invalid resource type for model: %s"), jen.Id("resourceType")))
			})
		})
	})

	//A function which returns the GORM table name for a FHIRResource when provided the FhirResource type string
	//uses a switch statement to return the correct type
	utilsFile.Comment("Returns the GORM table name for a FHIRResource when provided the FhirResource type string")
	utilsFile.Func().Id("GetTableNameByResourceType").Params(jen.Id("resourceType").String()).Params(jen.String(), jen.Error()).BlockFunc(func(g *jen.Group) {
		g.Switch(jen.Id("resourceType")).BlockFunc(func(s *jen.Group) {
			for _, resourceName := range AllowedResources {
				s.Case(jen.Lit(resourceName)).BlockFunc(func(c *jen.Group) {
					c.Return(jen.Lit(strcase.ToSnake("Fhir"+resourceName)), jen.Nil())
				})
			}
			s.Default().BlockFunc(func(d *jen.Group) {
				d.Return(jen.Lit(""), jen.Qual("fmt", "Errorf").Call(jen.Lit("Invalid resource type for table name: %s"), jen.Id("resourceType")))
			})
		})
	})

	//A function which returns all allowed resource types
	utilsFile.Comment("Returns a slice of all allowed resource types")
	utilsFile.Func().Id("GetAllowedResourceTypes").Params().Params(jen.Index().String()).BlockFunc(func(g *jen.Group) {
		g.Return(jen.Index().String().ValuesFunc(func(g *jen.Group) {
			for _, resourceName := range AllowedResources {
				g.Lit(resourceName)
			}
		}))
	})

	// Save the generated Go code to a file
	err = utilsFile.Save("utils.go")
	if err != nil {
		log.Fatal(err)
	}

}

//TODO: should we do this, or allow all resources instead of just USCore?
//The dataabase would be full of empty data, but we'd be more flexible & future-proof.. supporting other countries, etc.
var AllowedResources = []string{
	"Account",
	"AdverseEvent",
	"AllergyIntolerance",
	"Appointment",
	"Binary",
	"CarePlan",
	"CareTeam",
	"Claim",
	"ClaimResponse",
	"Composition",
	"Condition",
	"Consent",
	"Coverage",
	"CoverageEligibilityRequest",
	"CoverageEligibilityResponse",
	"Device",
	"DeviceRequest",
	"DiagnosticReport",
	"DocumentManifest",
	"DocumentReference",
	"Encounter",
	"Endpoint",
	"EnrollmentRequest",
	"EnrollmentResponse",
	"ExplanationOfBenefit",
	"FamilyMemberHistory",
	"Goal",
	"ImagingStudy",
	"Immunization",
	"InsurancePlan",
	"Location",
	"Media",
	"Medication",
	"MedicationAdministration",
	"MedicationDispense",
	"MedicationRequest",
	"MedicationStatement",
	"NutritionOrder",
	"Observation",
	"Organization",
	"OrganizationAffiliation",
	"Patient",
	"Person",
	"Practitioner",
	"PractitionerRole",
	"Procedure",
	"Provenance",
	"Questionnaire",
	"QuestionnaireResponse",
	"RelatedPerson",
	"Schedule",
	"ServiceRequest",
	"Slot",
	"Specimen",
	"VisionPrescription",
}

//simple field types are not json encoded in the DB and are always single values (not arrays)
func isSimpleFieldType(fieldType string) bool {
	switch fieldType {
	case "number", "uri", "date":
		return true
	case "token", "reference", "special", "quantity", "string":
		return false
	default:
		return true
	}
	return true
}

//https://hl7.org/fhir/search.html#token
//https://hl7.org/fhir/r4/valueset-search-param-type.html
func mapFieldType(fieldType string) string {
	switch fieldType {
	case "number":
		return "float64"
	case "token":
		return "gorm.io/datatypes#JSON"
	case "reference":
		return "gorm.io/datatypes#JSON"
	case "date":
		return "time#Time"
	case "string":
		return "gorm.io/datatypes#JSON"
	case "uri":
		return "string"
	case "special":
		return "gorm.io/datatypes#JSON"
	case "quantity":
		return "gorm.io/datatypes#JSON"
	default:
		return "string"
	}
}

//https://www.sqlite.org/datatype3.html
func mapGormType(fieldType string) string {
	// gorm:"type:text;serializer:json"

	switch fieldType {
	case "number":
		return "type:real"
	case "token":
		return "type:text;serializer:json"
	case "reference":
		return "type:text;serializer:json"
	case "date":
		return "type:datetime"
	case "string":
		return "type:text;serializer:json"
	case "uri":
		return "type:text"
	case "special":
		return "type:text;serializer:json"
	case "quantity":
		return "type:text;serializer:json"
	default:
		return "type:text"
	}
}
