package reviser

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/incu6us/goimports-reviser/v3/pkg/astutil"
	"github.com/incu6us/goimports-reviser/v3/pkg/std"
)

const (
	StandardInput        = "<standard-input>"
	stringValueSeparator = ","
)

var (
	codeGeneratedPattern = regexp.MustCompile(`^// Code generated .* DO NOT EDIT\.$`)
)

// SourceFile main struct for fixing an existing code
type SourceFile struct {
	shouldRemoveUnusedImports      bool
	shouldUseAliasForVersionSuffix bool
	shouldFormatCode               bool
	shouldSkipAutoGenerated        bool
	companyPackagePrefixes         []string
	importsOrders                  ImportsOrders

	projectName string
	filePath    string
}

// NewSourceFile constructor
func NewSourceFile(projectName, filePath string) *SourceFile {
	return &SourceFile{
		projectName: projectName,
		filePath:    filePath,
	}
}

// Fix is for revise imports and format the code
func (f *SourceFile) Fix(options ...SourceFileOption) ([]byte, bool, error) {
	for _, option := range options {
		err := option(f)
		if err != nil {
			return nil, false, err
		}
	}

	var originalContent []byte
	var err error
	if f.filePath == StandardInput {
		originalContent, err = io.ReadAll(os.Stdin)
	} else {
		originalContent, err = os.ReadFile(f.filePath)
	}
	if err != nil {
		return nil, false, err
	}

	fset := token.NewFileSet()

	pf, err := parser.ParseFile(fset, "", originalContent, parser.ParseComments)
	if err != nil {
		return nil, false, err
	}

	if f.shouldSkipAutoGenerated && isFileAutoGenerate(pf) {
		return originalContent, false, nil
	}

	importsWithMetadata, err := f.parseImports(pf)
	if err != nil {
		return nil, false, err
	}

	stdImports, generalImports, namedImports, projectLocalPkgs, projectImports := groupImports(
		f.projectName,
		f.companyPackagePrefixes,
		importsWithMetadata,
	)

	decls, ok := hasMultipleImportDecls(pf)
	if ok {
		pf.Decls = decls
	}

	f.fixImports(pf, stdImports, generalImports, namedImports, projectLocalPkgs, projectImports, importsWithMetadata)

	f.formatDecls(pf)

	fixedImportsContent, err := generateFile(fset, pf)
	if err != nil {
		return nil, false, err
	}

	formattedContent, err := format.Source(fixedImportsContent)
	if err != nil {
		return nil, false, err
	}

	return formattedContent, !bytes.Equal(originalContent, formattedContent), nil
}

func isFileAutoGenerate(pf *ast.File) bool {
	for _, comment := range pf.Comments {
		for _, c := range comment.List {
			if codeGeneratedPattern.MatchString(c.Text) && c.Pos() < pf.Package {
				return true
			}
		}
	}
	return false
}

func (f *SourceFile) formatDecls(file *ast.File) {
	if !f.shouldFormatCode {
		return
	}

	for _, decl := range file.Decls {
		switch dd := decl.(type) {
		case *ast.GenDecl:
			dd.Doc = fixCommentGroup(dd.Doc)
		case *ast.FuncDecl:
			dd.Doc = fixCommentGroup(dd.Doc)
		}
	}
}

func fixCommentGroup(commentGroup *ast.CommentGroup) *ast.CommentGroup {
	if commentGroup == nil {
		formattedDoc := &ast.CommentGroup{
			List: []*ast.Comment{},
		}

		return formattedDoc
	}

	formattedDoc := &ast.CommentGroup{
		List: make([]*ast.Comment, len(commentGroup.List)),
	}

	copy(formattedDoc.List, commentGroup.List)

	return formattedDoc
}

func groupImports(
	projectName string,
	localPkgPrefixes []string,
	importsWithMetadata map[string]*commentsMetadata,
) ([]string, []string, []string, []string, []string) {
	var (
		stdImports       []string
		projectImports   []string
		projectLocalPkgs []string
		namedImports     []string
		generalImports   []string
	)

	for imprt := range importsWithMetadata {
		values := strings.Split(imprt, " ")
		if len(values) > 1 {
			namedImports = append(namedImports, imprt)
			continue
		}

		pkgWithoutAlias := skipPackageAlias(imprt)

		if _, ok := std.StdPackages[pkgWithoutAlias]; ok {
			stdImports = append(stdImports, imprt)
			continue
		}

		var isLocalPackageFound bool
		for _, localPackagePrefix := range localPkgPrefixes {
			fmt.Printf("pkgWithoutAlias: %s localPackagePrefix: %s\n", pkgWithoutAlias, localPackagePrefix)
			if strings.HasPrefix(pkgWithoutAlias, localPackagePrefix) { // && !strings.HasPrefix(pkgWithoutAlias, projectName) {
				fmt.Printf("local package found: %s\n", imprt)
				projectLocalPkgs = append(projectLocalPkgs, imprt)
				isLocalPackageFound = true
				break
			}
		}

		if isLocalPackageFound {
			continue
		}

		if strings.Contains(pkgWithoutAlias, projectName) {
			projectImports = append(projectImports, imprt)
			continue
		}

		generalImports = append(generalImports, imprt)
	}

	sort.Strings(stdImports)
	sort.Strings(generalImports)
	sort.Strings(namedImports)
	sort.Strings(projectLocalPkgs)
	sort.Strings(projectImports)

	return stdImports, generalImports, namedImports, projectLocalPkgs, projectImports
}

func skipPackageAlias(pkg string) string {
	values := strings.Split(pkg, " ")
	if len(values) > 1 {
		return strings.Trim(values[1], `"`)
	}

	return strings.Trim(pkg, `"`)
}

func generateFile(fset *token.FileSet, f *ast.File) ([]byte, error) {
	var output []byte
	buffer := bytes.NewBuffer(output)
	if err := printer.Fprint(buffer, fset, f); err != nil {
		return nil, err
	}

	return buffer.Bytes(), nil
}

func isSingleCgoImport(dd *ast.GenDecl) bool {
	if dd.Tok != token.IMPORT {
		return false
	}
	if len(dd.Specs) != 1 {
		return false
	}
	return dd.Specs[0].(*ast.ImportSpec).Path.Value == `"C"`
}

func (f *SourceFile) fixImports(
	file *ast.File,
	stdImports, generalImports, namedImports, projectLocalPkgs, projectImports []string,
	commentsMetadata map[string]*commentsMetadata,
) {
	var importsPositions []*importPosition
	for _, decl := range file.Decls {
		dd, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}

		if dd.Tok != token.IMPORT || isSingleCgoImport(dd) {
			continue
		}

		importsPositions = append(
			importsPositions, &importPosition{
				Start: dd.Pos(),
				End:   dd.End(),
			},
		)

		fmt.Printf("named: %v\n", namedImports)
		one, two, three, four, five := f.importsOrders.sortImportsByOrder(stdImports, generalImports, namedImports, projectLocalPkgs, projectImports)
		dd.Specs = rebuildImports(dd.Tok, commentsMetadata, one, two, three, four, five)
	}

	clearImportDocs(file, importsPositions)
	removeEmptyImportNode(file)
}

// hasMultipleImportDecls will return combined import declarations to single declaration
//
// Ex.:
// import "fmt"
// import "io"
// -----
// to
// -----
// import (
//
//	"fmt"
//	"io"
//
// )
func hasMultipleImportDecls(f *ast.File) ([]ast.Decl, bool) {
	importSpecs := make([]ast.Spec, 0, len(f.Imports))
	for _, importSpec := range f.Imports {
		importSpecs = append(importSpecs, importSpec)
	}

	var (
		hasMultipleImportDecls   bool
		isFirstImportDeclDefined bool
	)

	decls := make([]ast.Decl, 0, len(f.Decls))
	for _, decl := range f.Decls {
		dd, ok := decl.(*ast.GenDecl)
		if !ok {
			decls = append(decls, decl)
			continue
		}

		if dd.Tok != token.IMPORT || isSingleCgoImport(dd) {
			decls = append(decls, dd)
			continue
		}

		if isFirstImportDeclDefined {
			hasMultipleImportDecls = true
			storedGenDecl := decls[len(decls)-1].(*ast.GenDecl)
			if storedGenDecl.Tok == token.IMPORT {
				storedGenDecl.Rparen = dd.End()
			}
			continue
		}

		dd.Specs = importSpecs
		decls = append(decls, dd)
		isFirstImportDeclDefined = true
	}

	return decls, hasMultipleImportDecls
}

func removeEmptyImportNode(f *ast.File) {
	var (
		decls      []ast.Decl
		hasImports bool
	)

	for _, decl := range f.Decls {
		dd, ok := decl.(*ast.GenDecl)
		if !ok {
			decls = append(decls, decl)

			continue
		}

		if dd.Tok == token.IMPORT && len(dd.Specs) > 0 {
			hasImports = true

			break
		}

		if dd.Tok != token.IMPORT {
			decls = append(decls, decl)
		}
	}

	if !hasImports {
		f.Decls = decls
	}
}

func rebuildImports(
	tok token.Token,
	commentsMetadata map[string]*commentsMetadata,
	firstImportGroup []string,
	secondImportsGroup []string,
	thirdImportsGroup []string,
	fourthImportGroup []string,
	fifthImportGroup []string,
) []ast.Spec {
	var specs []ast.Spec

	linesCounter := len(firstImportGroup)
	for _, imprt := range firstImportGroup {
		spec := &ast.ImportSpec{
			Path: &ast.BasicLit{Value: importWithComment(imprt, commentsMetadata), Kind: tok},
		}
		specs = append(specs, spec)

		linesCounter--

		if linesCounter == 0 && (len(secondImportsGroup) > 0 || len(thirdImportsGroup) > 0 || len(fourthImportGroup) > 0) {
			spec = &ast.ImportSpec{Path: &ast.BasicLit{Value: "", Kind: token.STRING}}

			specs = append(specs, spec)
		}
	}

	linesCounter = len(secondImportsGroup)
	for _, imprt := range secondImportsGroup {
		spec := &ast.ImportSpec{
			Path: &ast.BasicLit{Value: importWithComment(imprt, commentsMetadata), Kind: tok},
		}
		specs = append(specs, spec)

		linesCounter--

		if linesCounter == 0 && (len(thirdImportsGroup) > 0 || len(fourthImportGroup) > 0) {
			spec = &ast.ImportSpec{Path: &ast.BasicLit{Value: "", Kind: token.STRING}}

			specs = append(specs, spec)
		}
	}

	linesCounter = len(thirdImportsGroup)
	for _, imprt := range thirdImportsGroup {
		spec := &ast.ImportSpec{
			Path: &ast.BasicLit{Value: importWithComment(imprt, commentsMetadata), Kind: tok},
		}
		specs = append(specs, spec)

		linesCounter--

		if linesCounter == 0 && len(fourthImportGroup) > 0 {
			spec = &ast.ImportSpec{Path: &ast.BasicLit{Value: "", Kind: token.STRING}}

			specs = append(specs, spec)
		}
	}

	linesCounter = len(fourthImportGroup)
	for _, imprt := range fourthImportGroup {
		spec := &ast.ImportSpec{
			Path: &ast.BasicLit{Value: importWithComment(imprt, commentsMetadata), Kind: tok},
		}
		specs = append(specs, spec)

		linesCounter--

		if linesCounter == 0 && len(fourthImportGroup) > 0 {
			spec = &ast.ImportSpec{Path: &ast.BasicLit{Value: "", Kind: token.STRING}}

			specs = append(specs, spec)
		}
	}

	for _, imprt := range fifthImportGroup {
		spec := &ast.ImportSpec{
			Path: &ast.BasicLit{Value: importWithComment(imprt, commentsMetadata), Kind: tok},
		}
		specs = append(specs, spec)
	}

	return specs
}

func clearImportDocs(f *ast.File, importsPositions []*importPosition) {
	importsComments := make([]*ast.CommentGroup, 0, len(f.Comments))

	for _, comment := range f.Comments {
		for _, importPosition := range importsPositions {
			if importPosition.IsInRange(comment) {
				continue
			}
			importsComments = append(importsComments, comment)
		}
	}

	if len(f.Imports) > 0 {
		f.Comments = importsComments
	}
}

func importWithComment(imprt string, commentsMetadata map[string]*commentsMetadata) string {
	var comment string
	commentGroup, ok := commentsMetadata[imprt]
	if ok && commentGroup != nil && commentGroup.Comment != nil {
		for _, c := range commentGroup.Comment.List {
			comment += c.Text
		}
	}

	if comment == "" {
		return imprt
	}

	return fmt.Sprintf("%s %s", imprt, comment)
}

func (f *SourceFile) parseImports(file *ast.File) (map[string]*commentsMetadata, error) {
	importsWithMetadata := map[string]*commentsMetadata{}

	shouldRemoveUnusedImports := f.shouldRemoveUnusedImports
	shouldUseAliasForVersionSuffix := f.shouldUseAliasForVersionSuffix

	var packageImports map[string]string
	var err error

	if shouldRemoveUnusedImports || shouldUseAliasForVersionSuffix {
		packageImports, err = astutil.LoadPackageDependencies(path.Dir(f.filePath), astutil.ParseBuildTag(file))
		if err != nil {
			return nil, err
		}
	}

	for _, decl := range file.Decls {
		switch decl.(type) {
		case *ast.GenDecl:
			dd := decl.(*ast.GenDecl)
			if isSingleCgoImport(dd) {
				continue
			}
			if dd.Tok == token.IMPORT {
				for _, spec := range dd.Specs {
					var importSpecStr string
					importSpec := spec.(*ast.ImportSpec)

					if shouldRemoveUnusedImports && !astutil.UsesImport(
						file, packageImports, strings.Trim(importSpec.Path.Value, `"`),
					) {
						continue
					}

					if importSpec.Name != nil {
						importSpecStr = strings.Join([]string{importSpec.Name.String(), importSpec.Path.Value}, " ")
					} else {
						if shouldUseAliasForVersionSuffix {
							importSpecStr = setAliasForVersionedImportSpec(importSpec, packageImports)
						} else {
							importSpecStr = importSpec.Path.Value
						}
					}

					importsWithMetadata[importSpecStr] = &commentsMetadata{
						Doc:     importSpec.Doc,
						Comment: importSpec.Comment,
					}
				}
			}
		}
	}

	return importsWithMetadata, nil
}

func setAliasForVersionedImportSpec(importSpec *ast.ImportSpec, packageImports map[string]string) string {
	var importSpecStr string

	imprt := strings.Trim(importSpec.Path.Value, `"`)
	aliasName := packageImports[imprt]

	importSuffix := path.Base(imprt)
	if importSuffix != aliasName {
		importSpecStr = fmt.Sprintf("%s %s", aliasName, importSpec.Path.Value)
	} else {
		importSpecStr = importSpec.Path.Value
	}

	return importSpecStr
}

type commentsMetadata struct {
	Doc     *ast.CommentGroup
	Comment *ast.CommentGroup
}

type importPosition struct {
	Start token.Pos
	End   token.Pos
}

func (p *importPosition) IsInRange(comment *ast.CommentGroup) bool {
	if p.Start <= comment.Pos() && comment.Pos() <= p.End {
		return true
	}

	return false
}
